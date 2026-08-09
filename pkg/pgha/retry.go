package pgha

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// errStandby is what WaitForWritable's probe returns when it reached a
// node that is in recovery — a real answer from a real database, just not
// from the leader. It is unexported and reported as retryable, so callers
// see it only as "still waiting".
var errStandby = errors.New("connected node is a standby, not the leader")

const (
	// retryBudget bounds every Retry call. It has to comfortably exceed a
	// worst-case failover, or startup would give up in the middle of one
	// and turn a recoverable moment into a crash loop: detection alone
	// costs up to Patroni's ttl (15s — see infra/patroni/patroni.yml),
	// plus promotion, plus HAProxy noticing the new leader. Two minutes
	// is far more than that and still short enough that a genuinely
	// misconfigured DATABASE_URL fails while someone is still watching.
	retryBudget = 2 * time.Minute

	// retryBaseDelay/retryMaxDelay shape the backoff between attempts:
	// fast at first, because most retries here are a failover that is
	// already nearly over, then settling to a steady poll rather than
	// hammering a cluster that is busy electing a leader.
	retryBaseDelay = 250 * time.Millisecond
	retryMaxDelay  = 5 * time.Second

	// attemptTimeout bounds a single attempt, so one call that hangs
	// (a TCP connection to a node that vanished rather than refused)
	// cannot consume the whole budget by itself.
	attemptTimeout = 10 * time.Second
)

// Retry runs fn until it succeeds, until it fails with an error that
// retrying cannot fix, or until retryBudget expires.
//
// The classification is the whole point. Retrying everything would mean a
// genuine error — a migration with a syntax error, a missing table, a
// wrong password — takes two minutes to surface instead of failing
// immediately, and arrives buried under a stack of identical log lines.
// Retrying nothing is what the code did before this package existed, and
// meant any service that happened to restart during a failover died on
// its first migration attempt. So only errors that describe an
// unreachable or not-yet-promoted database are retried; everything else
// is returned on the spot.
//
// logf is called once per failed attempt, so an operator watching a
// service start during a failover sees progress rather than silence. Pass
// log.Printf.
func Retry(ctx context.Context, opName string, logf func(string, ...any), fn func(context.Context) error) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}

	deadline := time.Now().Add(retryBudget)
	delay := retryBaseDelay

	for attempt := 1; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		err := fn(attemptCtx)
		// Whether THIS attempt's deadline fired, as opposed to the
		// caller's. Captured before cancel(), and it matters: an attempt
		// that hit attemptTimeout is by definition something that hung
		// rather than something that is wrong, so it is retryable — but
		// the error it produced is indistinguishable from a caller
		// deadline once it leaves this function. Deciding here, where
		// both contexts are still in scope, is the only place the
		// distinction is available.
		attemptTimedOut := attemptCtx.Err() != nil && ctx.Err() == nil
		cancel()

		if err == nil {
			if attempt > 1 {
				logf("pgha: %s: succeeded on attempt %d", opName, attempt)
			}
			return nil
		}

		if !attemptTimedOut && !errors.Is(err, errStandby) && !IsUnavailable(err) {
			return err
		}

		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w", opName, ctx.Err())
		}
		if remaining := time.Until(deadline); remaining <= 0 {
			return fmt.Errorf("%s: gave up after %s and %d attempts: %w", opName, retryBudget, attempt, err)
		} else if delay > remaining {
			delay = remaining
		}

		logf("pgha: %s: attempt %d failed, retrying in %s: %v", opName, attempt, delay, err)

		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", opName, ctx.Err())
		case <-time.After(delay):
		}

		if delay *= 2; delay > retryMaxDelay {
			delay = retryMaxDelay
		}
	}
}
