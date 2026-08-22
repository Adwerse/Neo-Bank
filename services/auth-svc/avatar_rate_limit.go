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

// recordAvatarUploadAttempt is POST /profile/avatar/upload-url's per-user
// rate limit: without it, this endpoint is a way to mint an unbounded
// number of presigned upload targets and fill the storage bucket with
// garbage, since issuing a URL costs nothing server-side and there's no
// other limit on how many any one user can request. Counts userID's
// attempts in the trailing window and, only if that count is still under
// limit, atomically records this one too; returns false (without
// recording anything) once the limit is reached.
//
// Same single-statement count-then-conditionally-insert idiom as
// accounts-svc's recordResolveAttempt (services/accounts-svc/rate_limit.go)
// — a separate SELECT then INSERT would let two concurrent requests from
// the same user both read "under limit" before either writes.
func recordAvatarUploadAttempt(ctx context.Context, pool *pgxpool.Pool, userID string, limit int, window time.Duration) (bool, error) {
	var id string
	err := pool.QueryRow(ctx, `
		WITH recent_count AS (
			SELECT count(*) AS n FROM avatar_upload_attempts
			WHERE user_id = $1 AND attempted_at > now() - make_interval(secs => $2)
		)
		INSERT INTO avatar_upload_attempts (user_id)
		SELECT $1 FROM recent_count WHERE recent_count.n < $3
		RETURNING id`,
		userID, window.Seconds(), limit,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("record avatar upload attempt: %w", err)
	}
	return true, nil
}

// avatarUploadAttemptsCleanupInterval/Retention keep avatar_upload_attempts
// from growing forever — same reasoning and shape as accounts-svc's
// resolveAttemptsCleanupInterval/Retention.
const (
	avatarUploadAttemptsCleanupInterval = 10 * time.Minute
	avatarUploadAttemptsRetention       = 1 * time.Hour
)

// runAvatarUploadAttemptsCleanupWorker periodically deletes
// avatar_upload_attempts rows older than avatarUploadAttemptsRetention.
// Mirrors accounts-svc's runResolveAttemptsCleanupWorker exactly — same
// class of problem (a rate-limit attempt log with no other retention
// mechanism), different table.
func runAvatarUploadAttemptsCleanupWorker(ctx context.Context, pool *pgxpool.Pool) {
	ticker := time.NewTicker(avatarUploadAttemptsCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := deleteStaleAvatarUploadAttempts(ctx, pool, avatarUploadAttemptsRetention)
			if err != nil {
				log.Printf("auth-svc: avatar_upload_attempts cleanup: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("auth-svc: avatar_upload_attempts cleanup: deleted %d stale row(s)", n)
			}
		}
	}
}

// deleteStaleAvatarUploadAttempts removes avatar_upload_attempts rows
// older than retention, returning how many were deleted. Split out from
// the ticker loop so it can be exercised directly by a test.
func deleteStaleAvatarUploadAttempts(ctx context.Context, pool *pgxpool.Pool, retention time.Duration) (int64, error) {
	tag, err := pool.Exec(ctx, "DELETE FROM avatar_upload_attempts WHERE attempted_at < now() - make_interval(secs => $1)", retention.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
