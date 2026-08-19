package main

import (
	"context"
	crand "crypto/rand"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// newTestPool connects to the postgres instance from docker-compose.yml via
// DATABASE_URL, skipping the test if it isn't set — same convention as
// transfers-svc's transfer_test.go.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set; skipping test that requires a live postgres (see docker-compose.yml)")
	}

	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// randomUUIDForTest mints a UUID v4 by hand — same convention as
// transfers-svc's transfer_test.go.
func randomUUIDForTest(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		t.Fatalf("generate random uuid: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func deleteResolveAttempts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "DELETE FROM iban_resolve_attempts WHERE user_id = $1", userID); err != nil {
		t.Logf("cleanup: delete iban_resolve_attempts user_id=%s: %v", userID, err)
	}
}

func countResolveAttempts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM iban_resolve_attempts WHERE user_id = $1", userID).Scan(&count); err != nil {
		t.Fatalf("count iban_resolve_attempts: %v", err)
	}
	return count
}

// TestRecordResolveAttempt_AllowsUpToLimitThenBlocks pins the core rate
// limit contract: exactly `limit` attempts succeed (and are recorded),
// and every attempt after that is rejected without being recorded — a
// blocked attempt must not itself count towards ever un-blocking.
func TestRecordResolveAttempt_AllowsUpToLimitThenBlocks(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	userID := randomUUIDForTest(t)
	t.Cleanup(func() { deleteResolveAttempts(t, ctx, pool, userID) })

	const limit = 3
	window := time.Minute

	for i := 0; i < limit; i++ {
		allowed, err := recordResolveAttempt(ctx, pool, userID, limit, window)
		if err != nil {
			t.Fatalf("recordResolveAttempt (attempt %d): unexpected error: %v", i, err)
		}
		if !allowed {
			t.Fatalf("recordResolveAttempt (attempt %d) = false, want true (within limit)", i)
		}
	}

	for i := 0; i < 3; i++ {
		allowed, err := recordResolveAttempt(ctx, pool, userID, limit, window)
		if err != nil {
			t.Fatalf("recordResolveAttempt (over limit, call %d): unexpected error: %v", i, err)
		}
		if allowed {
			t.Fatalf("recordResolveAttempt (over limit, call %d) = true, want false", i)
		}
	}

	if got := countResolveAttempts(t, ctx, pool, userID); got != limit {
		t.Errorf("iban_resolve_attempts rows = %d, want %d (blocked calls must not be recorded)", got, limit)
	}
}

// TestRecordResolveAttempt_WindowExpiry confirms attempts outside the
// trailing window don't count against the limit — backdates existing rows
// past the window (same technique as transfers-svc's
// setTransferCreatedAtForTest) rather than waiting in real time.
func TestRecordResolveAttempt_WindowExpiry(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	userID := randomUUIDForTest(t)
	t.Cleanup(func() { deleteResolveAttempts(t, ctx, pool, userID) })

	const limit = 2
	window := time.Minute

	for i := 0; i < limit; i++ {
		allowed, err := recordResolveAttempt(ctx, pool, userID, limit, window)
		if err != nil || !allowed {
			t.Fatalf("recordResolveAttempt (seed attempt %d): allowed=%v err=%v", i, allowed, err)
		}
	}
	// Confirm the limit is actually reached before backdating.
	if allowed, err := recordResolveAttempt(ctx, pool, userID, limit, window); err != nil || allowed {
		t.Fatalf("recordResolveAttempt (pre-backdate check): allowed=%v err=%v, want false", allowed, err)
	}

	if _, err := pool.Exec(ctx, "UPDATE iban_resolve_attempts SET attempted_at = now() - interval '10 minutes' WHERE user_id = $1", userID); err != nil {
		t.Fatalf("backdate attempts: %v", err)
	}

	allowed, err := recordResolveAttempt(ctx, pool, userID, limit, window)
	if err != nil {
		t.Fatalf("recordResolveAttempt (after window expiry): unexpected error: %v", err)
	}
	if !allowed {
		t.Error("recordResolveAttempt (after window expiry) = false, want true (old attempts must not count)")
	}
}

// TestDeleteStaleResolveAttempts confirms the cleanup sweep only removes
// rows past retention, leaving recent ones untouched.
func TestDeleteStaleResolveAttempts(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	userID := randomUUIDForTest(t)
	t.Cleanup(func() { deleteResolveAttempts(t, ctx, pool, userID) })

	if allowed, err := recordResolveAttempt(ctx, pool, userID, 10, time.Minute); err != nil || !allowed {
		t.Fatalf("seed recent attempt: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := recordResolveAttempt(ctx, pool, userID, 10, time.Minute); err != nil || !allowed {
		t.Fatalf("seed stale attempt: allowed=%v err=%v", allowed, err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE iban_resolve_attempts SET attempted_at = now() - interval '2 hours'
		 WHERE user_id = $1 AND id = (SELECT id FROM iban_resolve_attempts WHERE user_id = $1 ORDER BY id LIMIT 1)`,
		userID,
	); err != nil {
		t.Fatalf("backdate one attempt: %v", err)
	}

	deleted, err := deleteStaleResolveAttempts(ctx, pool, time.Hour)
	if err != nil {
		t.Fatalf("deleteStaleResolveAttempts: unexpected error: %v", err)
	}
	if deleted < 1 {
		t.Errorf("deleteStaleResolveAttempts deleted %d rows, want at least 1", deleted)
	}
	if got := countResolveAttempts(t, ctx, pool, userID); got != 1 {
		t.Errorf("iban_resolve_attempts rows remaining = %d, want 1 (the recent one)", got)
	}
}
