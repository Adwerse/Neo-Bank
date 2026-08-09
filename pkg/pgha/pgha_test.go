package pgha

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestIsUnavailable_Classification pins the one decision this package
// makes that has a consequence beyond a log line: notifications-svc uses
// it to decide whether a failed transfer.events message is poison (→ dead
// letter topic) or just caught a failover (→ keep retrying). Getting the
// "not unavailable" column wrong means retrying poison forever; getting
// the "unavailable" column wrong means DLQing good messages during every
// failover.
func TestIsUnavailable_Classification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},

		// The failover-shaped errors.
		{"admin_shutdown 57P01", &pgconn.PgError{Code: "57P01"}, true},
		{"crash_shutdown 57P02", &pgconn.PgError{Code: "57P02"}, true},
		{"cannot_connect_now 57P03", &pgconn.PgError{Code: "57P03"}, true},
		{"connection_failure 08006", &pgconn.PgError{Code: "08006"}, true},
		{"read_only_sql_transaction 25006", &pgconn.PgError{Code: "25006"}, true},
		{"eof", io.EOF, true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
		{"dial refused", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{"conn closed text", errors.New("conn closed"), true},

		// Wrapped, because every real call site wraps: the consumer
		// returns fmt.Errorf("upsert user_contacts: %w", err) and the
		// classification has to survive that.
		{"wrapped admin_shutdown", fmt.Errorf("mark event processed: %w", &pgconn.PgError{Code: "57P01"}), true},
		{"double wrapped eof", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", io.EOF)), true},

		// Real errors, which must NOT be retried or they mask defects.
		{"undefined_table 42P01", &pgconn.PgError{Code: "42P01"}, false},
		{"syntax_error 42601", &pgconn.PgError{Code: "42601"}, false},
		{"unique_violation 23505", &pgconn.PgError{Code: "23505"}, false},
		{"invalid_password 28P01", &pgconn.PgError{Code: "28P01"}, false},
		{"check_violation 23514", &pgconn.PgError{Code: "23514"}, false},
		{"plain error", errors.New("something went wrong"), false},

		// Context errors are the caller's business, not a database
		// condition — see IsUnavailable's doc comment.
		{"deadline exceeded", context.DeadlineExceeded, false},
		{"canceled", context.Canceled, false},
		{"wrapped deadline exceeded", fmt.Errorf("query: %w", context.DeadlineExceeded), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsUnavailable(tt.err); got != tt.want {
				t.Errorf("IsUnavailable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestIsUnavailable_ConnectTimeoutIsNotACallerDeadline is a regression
// test for a bug that crash-looped every service the first time the stack
// came up against a Patroni cluster mid-election.
//
// pgx enforces its own ConnectTimeout, so a dial that never gets a
// response fails with a *pgconn.ConnectError whose error chain ends in
// context.DeadlineExceeded. An IsUnavailable that checks for context
// errors before checking for connect errors reads that as "the caller's
// deadline fired" and reports it non-retryable — so WaitForWritable gave
// up on its first attempt and main() called log.Fatalf, on a cluster that
// would have been ready seconds later.
//
// The error is produced for real rather than constructed, because
// pgconn.ConnectError's cause is unexported and a hand-built one could
// not have the shape that actually occurs. A listener that accepts the
// TCP connection and then never speaks the protocol reproduces it
// exactly: connected, but no startup response.
func TestIsUnavailable_ConnectTimeoutIsNotACallerDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	// Accept and hold. Closing the accepted connection would produce an
	// EOF instead of the timeout this test is about.
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()

	dsn := fmt.Sprintf("postgres://u:p@%s/db?sslmode=disable&connect_timeout=1", listener.Addr().String())
	_, err = pgconn.Connect(context.Background(), dsn)
	if err == nil {
		t.Fatal("expected the connect to time out against a listener that never responds")
	}

	var connErr *pgconn.ConnectError
	if !errors.As(err, &connErr) {
		t.Fatalf("expected a *pgconn.ConnectError, got %T: %v", err, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the chain to end in context.DeadlineExceeded (that is the trap being guarded), got: %v", err)
	}

	if !IsUnavailable(err) {
		t.Errorf("IsUnavailable(%v) = false, want true — a connect timeout is a dead node, not a caller deadline", err)
	}
}

// TestRetry_AttemptTimeoutIsRetryable covers the same trap one layer up.
// Retry imposes attemptTimeout on each call; when that fires, fn sees a
// context error that is genuinely a timeout of the attempt, not of the
// caller's own context, and retrying is the whole point.
func TestRetry_AttemptTimeoutIsRetryable(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), "test", nil, func(attemptCtx context.Context) error {
		calls++
		if calls == 1 {
			// Simulate an attempt that outlives its own deadline and
			// reports the context error, with nothing else to identify it.
			<-attemptCtx.Done()
			return attemptCtx.Err()
		}
		return nil
	})

	if err != nil {
		t.Fatalf("Retry returned %v, want nil — an attempt timeout must be retried", err)
	}
	if calls != 2 {
		t.Errorf("fn called %d times, want 2", calls)
	}
}

// TestRetry_ReturnsRealErrorsImmediately is the guard against the failure
// mode where a broken migration takes the full two-minute budget to
// surface. A 42P01 is not a failover, and Retry must not treat it as one.
func TestRetry_ReturnsRealErrorsImmediately(t *testing.T) {
	realErr := &pgconn.PgError{Code: "42P01", Message: "relation does not exist"}
	calls := 0

	start := time.Now()
	err := Retry(context.Background(), "test", nil, func(context.Context) error {
		calls++
		return realErr
	})
	elapsed := time.Since(start)

	if !errors.Is(err, realErr) {
		t.Fatalf("Retry returned %v, want the original error", err)
	}
	if calls != 1 {
		t.Errorf("fn called %d times, want exactly 1 — a non-retryable error must not be retried", calls)
	}
	if elapsed > time.Second {
		t.Errorf("Retry took %s to return a non-retryable error, want immediate", elapsed)
	}
}

// TestRetry_RetriesUntilAvailable is the case a service restarted in the
// middle of a failover hits: the first few attempts fail because there is
// no leader yet, and then one succeeds.
func TestRetry_RetriesUntilAvailable(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), "test", nil, func(context.Context) error {
		calls++
		if calls < 3 {
			return &pgconn.PgError{Code: "57P03", Message: "the database system is starting up"}
		}
		return nil
	})

	if err != nil {
		t.Fatalf("Retry returned %v, want nil once fn succeeded", err)
	}
	if calls != 3 {
		t.Errorf("fn called %d times, want 3", calls)
	}
}

// TestRetry_StopsOnContextCancel proves shutdown wins over the retry
// budget — otherwise a service told to stop during a failover would sit
// in Retry for up to two minutes before noticing.
func TestRetry_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0

	start := time.Now()
	err := Retry(ctx, "test", nil, func(context.Context) error {
		calls++
		if calls == 2 {
			cancel()
		}
		return io.EOF
	})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Retry returned %v, want a context.Canceled", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Retry took %s to notice cancellation", elapsed)
	}
}

// TestRetry_StandbyIsRetryable covers WaitForWritable's specific case:
// the probe reached a real database that answered correctly, and the
// answer was "I am a standby". That is not an error to report, it is a
// reason to wait — HAProxy is about to route elsewhere.
func TestRetry_StandbyIsRetryable(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), "test", nil, func(context.Context) error {
		calls++
		if calls < 2 {
			return errStandby
		}
		return nil
	})

	if err != nil {
		t.Fatalf("Retry returned %v, want nil", err)
	}
	if calls != 2 {
		t.Errorf("fn called %d times, want 2", calls)
	}
}

// TestNewPool_AppliesFailoverDefaults checks the pool settings that make
// a departed leader's connections get discarded promptly rather than
// handed out to callers for the next minute.
func TestNewPool_AppliesFailoverDefaults(t *testing.T) {
	// pgxpool.New does no I/O, so this needs no database.
	pool, err := NewPool(context.Background(), "postgres://u:p@127.0.0.1:1/db?sslmode=disable")
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	config := pool.Config()
	if config.HealthCheckPeriod != healthCheckPeriod {
		t.Errorf("HealthCheckPeriod = %s, want %s", config.HealthCheckPeriod, healthCheckPeriod)
	}
	if config.ConnConfig.ConnectTimeout != connectTimeout {
		t.Errorf("ConnectTimeout = %s, want %s", config.ConnConfig.ConnectTimeout, connectTimeout)
	}
	if config.MaxConnLifetime != maxConnLifetime {
		t.Errorf("MaxConnLifetime = %s, want %s", config.MaxConnLifetime, maxConnLifetime)
	}
	if config.MaxConnLifetimeJitter != maxConnLifetimeJitter {
		t.Errorf("MaxConnLifetimeJitter = %s, want %s", config.MaxConnLifetimeJitter, maxConnLifetimeJitter)
	}
}

// TestNewPool_DSNWins guards the "settings in the DSN win" promise, so a
// connection string can still tune the pool without this package
// silently overriding it.
func TestNewPool_DSNWins(t *testing.T) {
	pool, err := NewPool(context.Background(), "postgres://u:p@127.0.0.1:1/db?sslmode=disable&pool_health_check_period=3s&pool_max_conns=7")
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	config := pool.Config()
	if config.HealthCheckPeriod != 3*time.Second {
		t.Errorf("HealthCheckPeriod = %s, want the DSN's 3s", config.HealthCheckPeriod)
	}
	if config.MaxConns != 7 {
		t.Errorf("MaxConns = %d, want the DSN's 7", config.MaxConns)
	}
}
