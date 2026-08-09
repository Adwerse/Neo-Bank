package pgha

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// unavailableSQLStates are the SQLSTATEs that mean "this database is not
// in a position to serve you right now", as opposed to "your query is
// wrong". Every one of them is something a failover produces:
//
//	08xxx — connection exception: the connection was lost, refused, or
//	        never completed. What the client sees when HAProxy tears down
//	        sessions to a node it just marked down.
//	57P01 — admin_shutdown: the server is shutting down. Patroni demoting
//	        or stopping a node produces this on every open session.
//	57P02 — crash_shutdown: another backend crashed and the server is
//	        restarting.
//	57P03 — cannot_connect_now: the server is starting up and not yet
//	        accepting connections. A promoted node in the seconds before
//	        it finishes recovery, and a rejoining old leader replaying WAL.
//	25006 — read_only_sql_transaction: a write reached a node that is in
//	        recovery. This is the "I was routed to a standby" error, and it
//	        is emphatically retryable — the same statement against the same
//	        address succeeds once routing catches up.
//	40001 — serialization_failure, and
//	40P01 — deadlock_detected: not failover-specific, but transient by
//	        definition and safe to retry for the idempotent startup work
//	        this package's Retry is used for.
var unavailableSQLStates = map[string]bool{
	"08000": true, // connection_exception
	"08001": true, // sqlclient_unable_to_establish_sqlconnection
	"08003": true, // connection_does_not_exist
	"08004": true, // sqlserver_rejected_establishment_of_sqlconnection
	"08006": true, // connection_failure
	"08007": true, // transaction_resolution_unknown
	"57P01": true, // admin_shutdown
	"57P02": true, // crash_shutdown
	"57P03": true, // cannot_connect_now
	"25006": true, // read_only_sql_transaction
	"40001": true, // serialization_failure
	"40P01": true, // deadlock_detected
}

// IsUnavailable reports whether err means the database was unreachable or
// not accepting the work — a condition that a retry a moment later can
// plausibly resolve — rather than a defect in the query or the data.
//
// It exists because "retry on any error" and "never retry" are both wrong
// in the two places that call it. In pgha.Retry, retrying everything
// would bury a real migration failure under two minutes of identical log
// lines. In notifications-svc's transfer.events consumer, NOT
// distinguishing the two is an actual data-handling bug: that loop gives
// a message a fixed number of attempts and then routes it to the dead
// letter topic as poison. A failover lasting longer than the retry ladder
// would exhaust those attempts on messages that are perfectly fine, and
// quietly DLQ a burst of real transfer notifications for no reason other
// than that Postgres was briefly electing a leader.
//
// Deliberately NOT treated as unavailable: context.DeadlineExceeded and
// context.Canceled. Both are ambiguous — a deadline fires the same way
// whether the server was unreachable or the query was simply slow — and
// callers own their own contexts, so a cancelled context means the caller
// asked to stop, which is not a thing to retry through.
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}

	// Checked BEFORE the context clause below, and the order is the whole
	// subtlety of this function.
	//
	// A failed connection attempt is always an infrastructure condition,
	// no matter what it failed with. pgx enforces its own ConnectTimeout
	// internally, so a node that is gone produces:
	//
	//   failed to connect to `user=neobank database=neobank`:
	//   172.19.0.6:5432 (pg-haproxy): failed to receive message:
	//   timeout: context deadline exceeded
	//
	// — a *pgconn.ConnectError whose chain ends in
	// context.DeadlineExceeded. Testing for the context error first (as
	// this function originally did) classifies that as "the caller's
	// deadline fired, not a database problem" and reports it as
	// non-retryable. The deadline that fired was pgx's own, on a dial
	// that never reached a server. That inversion is not theoretical:
	// it crash-looped every service on the first start against a cluster
	// that was still electing a leader, because WaitForWritable gave up
	// on attempt one.
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return true
	}

	// Now the context clause, for context errors that did NOT come from a
	// connect attempt — i.e. genuinely the caller's own deadline or
	// cancellation. It has to precede the net.Error branch below, because
	// context.DeadlineExceeded itself satisfies net.Error (it reports
	// Timeout() == true) and would otherwise be swallowed there.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}

	// A closed or reset connection surfaces as EOF from the protocol
	// reader before any PgError is available to inspect.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	// Dial failures, resets, and the connection-refused a node produces
	// while it is down.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return unavailableSQLStates[pgErr.Code]
	}

	// pgconn.SafeToRetry is true only for errors raised before the query
	// reached the server — a broken pooled connection being the common
	// case. It is checked last because it is the narrowest signal here,
	// but it catches the plain "conn closed" errors that carry no
	// SQLSTATE and no net.Error underneath.
	if pgconn.SafeToRetry(err) {
		return true
	}

	// Last resort, and the only string matching in this file. Two
	// separate reasons it cannot be avoided:
	//
	//  1. pgxpool reports a connection closed underneath a checked-out
	//     *Conn with a bare errors.New — no type, no SQLSTATE.
	//  2. golang-migrate wraps every error from a migration statement in
	//     its own database.Error, which formats the original error into a
	//     string field and implements no Unwrap(). errors.As cannot see
	//     through it, so a failover landing mid-migration is only
	//     recognisable by the text pgconn produced. (This is a
	//     defence-in-depth path: services call WaitForWritable before
	//     migrating, so the common case is caught by the typed checks
	//     above, on the ping that opens the migration connection.)
	//
	// Kept narrow, and last, so it can only ever add to a decision the
	// typed checks above did not already make.
	msg := err.Error()
	for _, fragment := range []string{
		"conn closed",
		"closed pool",
		"unexpected EOF",
		"server closed the connection unexpectedly",
		"failed to connect to",
		"the database system is shutting down",
		"the database system is starting up",
		"in a read-only transaction",
	} {
		if strings.Contains(msg, fragment) {
			return true
		}
	}
	return false
}
