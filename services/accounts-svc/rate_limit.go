package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// recordResolveAttempt is ResolveAccountByIban's per-user rate limit: an
// endpoint that answers "this account exists" / "it doesn't" is an
// enumeration oracle, and a checksum-valid IBAN only narrows the space
// worth guessing — it does not make guessing safe. Counts userID's
// attempts in the trailing window and, only if that count is still under
// limit, atomically records this one as well; returns false (without
// recording anything) once the limit is reached.
//
// The count-then-conditionally-insert is one statement, not a separate
// SELECT followed by an INSERT: two round trips would let two concurrent
// requests from the same user both read "under limit" before either
// writes, letting both through. A single CTE + INSERT ... SELECT ...
// WHERE closes that window down to Postgres's own MVCC snapshot
// guarantees for the statement — the same RETURNING-based idiom
// createAccountForUser's ON CONFLICT ... DO NOTHING RETURNING id uses
// elsewhere in this package for "insert, and tell me whether it happened."
func recordResolveAttempt(ctx context.Context, pool *pgxpool.Pool, userID string, limit int, window time.Duration) (bool, error) {
	var id string
	err := pool.QueryRow(ctx, `
		WITH recent_count AS (
			SELECT count(*) AS n FROM iban_resolve_attempts
			WHERE user_id = $1 AND attempted_at > now() - make_interval(secs => $2)
		)
		INSERT INTO iban_resolve_attempts (user_id)
		SELECT $1 FROM recent_count WHERE recent_count.n < $3
		RETURNING id`,
		userID, window.Seconds(), limit,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("record iban resolve attempt: %w", err)
	}
	return true, nil
}

// resolveAttemptsCleanupInterval and resolveAttemptsRetention keep
// iban_resolve_attempts from growing forever — every call to
// recordResolveAttempt inserts a row, and nothing else ever deletes one.
// retention only needs to safely outlive the longest rate-limit window any
// caller configures; an hour is generous headroom over the default 5m
// window (see main.go) without keeping rows around meaningfully longer
// than the data is useful for.
const (
	resolveAttemptsCleanupInterval = 10 * time.Minute
	resolveAttemptsRetention       = 1 * time.Hour
)

// runResolveAttemptsCleanupWorker periodically deletes iban_resolve_attempts
// rows older than resolveAttemptsRetention, mirroring pkg/outbox's
// RunCleanupWorker shape for the same class of problem (an
// attempt/event log table with no other retention mechanism). Not
// pkg/outbox itself: that package's cleanup is specific to the outbox
// table's schema (published_at), not a generic time-bounded sweep.
func runResolveAttemptsCleanupWorker(ctx context.Context, pool *pgxpool.Pool) {
	ticker := time.NewTicker(resolveAttemptsCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := deleteStaleResolveAttempts(ctx, pool, resolveAttemptsRetention)
			if err != nil {
				log.Printf("accounts-svc: iban_resolve_attempts cleanup: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("accounts-svc: iban_resolve_attempts cleanup: deleted %d stale row(s)", n)
			}
		}
	}
}

// deleteStaleResolveAttempts removes iban_resolve_attempts rows older than
// retention, returning how many were deleted. Split out from
// runResolveAttemptsCleanupWorker's ticker loop so it can be exercised
// directly by a test without waiting on resolveAttemptsCleanupInterval.
func deleteStaleResolveAttempts(ctx context.Context, pool *pgxpool.Pool, retention time.Duration) (int64, error) {
	tag, err := pool.Exec(ctx, "DELETE FROM iban_resolve_attempts WHERE attempted_at < now() - make_interval(secs => $1)", retention.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
