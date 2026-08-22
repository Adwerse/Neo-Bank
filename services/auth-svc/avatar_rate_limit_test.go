package main

import (
	"context"
	"testing"
	"time"
)

// TestRecordAvatarUploadAttempt_AllowsUpToLimitThenBlocks mirrors
// accounts-svc's TestRecordResolveAttempt_AllowsUpToLimitThenBlocks
// exactly (same CTE+INSERT idiom, same table shape): exactly `limit`
// attempts succeed and are recorded, and every attempt after that is
// rejected without being recorded.
func TestRecordAvatarUploadAttempt_AllowsUpToLimitThenBlocks(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	userID := randomUUID(t)
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, "DELETE FROM avatar_upload_attempts WHERE user_id = $1", userID); err != nil {
			t.Logf("cleanup: delete avatar_upload_attempts user_id=%s: %v", userID, err)
		}
	})

	const limit = 3
	window := time.Minute

	for i := 0; i < limit; i++ {
		allowed, err := recordAvatarUploadAttempt(ctx, pool, userID, limit, window)
		if err != nil {
			t.Fatalf("recordAvatarUploadAttempt (attempt %d): unexpected error: %v", i, err)
		}
		if !allowed {
			t.Fatalf("recordAvatarUploadAttempt (attempt %d) = false, want true (within limit)", i)
		}
	}

	for i := 0; i < 3; i++ {
		allowed, err := recordAvatarUploadAttempt(ctx, pool, userID, limit, window)
		if err != nil {
			t.Fatalf("recordAvatarUploadAttempt (over limit, call %d): unexpected error: %v", i, err)
		}
		if allowed {
			t.Fatalf("recordAvatarUploadAttempt (over limit, call %d) = true, want false", i)
		}
	}

	var count int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM avatar_upload_attempts WHERE user_id = $1", userID).Scan(&count); err != nil {
		t.Fatalf("count avatar_upload_attempts: %v", err)
	}
	if count != limit {
		t.Errorf("avatar_upload_attempts rows = %d, want %d (blocked calls must not be recorded)", count, limit)
	}
}

// TestRecordAvatarUploadAttempt_WindowExpiry confirms attempts outside the
// trailing window don't count against the limit.
func TestRecordAvatarUploadAttempt_WindowExpiry(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	userID := randomUUID(t)
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, "DELETE FROM avatar_upload_attempts WHERE user_id = $1", userID); err != nil {
			t.Logf("cleanup: delete avatar_upload_attempts user_id=%s: %v", userID, err)
		}
	})

	const limit = 2
	window := time.Minute

	for i := 0; i < limit; i++ {
		allowed, err := recordAvatarUploadAttempt(ctx, pool, userID, limit, window)
		if err != nil || !allowed {
			t.Fatalf("recordAvatarUploadAttempt (seed attempt %d): allowed=%v err=%v", i, allowed, err)
		}
	}
	if allowed, err := recordAvatarUploadAttempt(ctx, pool, userID, limit, window); err != nil || allowed {
		t.Fatalf("recordAvatarUploadAttempt (pre-backdate check): allowed=%v err=%v, want false", allowed, err)
	}

	if _, err := pool.Exec(ctx, "UPDATE avatar_upload_attempts SET attempted_at = now() - interval '10 minutes' WHERE user_id = $1", userID); err != nil {
		t.Fatalf("backdate attempts: %v", err)
	}

	allowed, err := recordAvatarUploadAttempt(ctx, pool, userID, limit, window)
	if err != nil {
		t.Fatalf("recordAvatarUploadAttempt (after window expiry): unexpected error: %v", err)
	}
	if !allowed {
		t.Error("recordAvatarUploadAttempt (after window expiry) = false, want true (old attempts must not count)")
	}
}

// TestDeleteStaleAvatarUploadAttempts confirms the cleanup sweep only
// removes rows past retention, leaving recent ones untouched.
func TestDeleteStaleAvatarUploadAttempts(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	userID := randomUUID(t)
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, "DELETE FROM avatar_upload_attempts WHERE user_id = $1", userID); err != nil {
			t.Logf("cleanup: delete avatar_upload_attempts user_id=%s: %v", userID, err)
		}
	})

	if allowed, err := recordAvatarUploadAttempt(ctx, pool, userID, 10, time.Minute); err != nil || !allowed {
		t.Fatalf("seed recent attempt: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := recordAvatarUploadAttempt(ctx, pool, userID, 10, time.Minute); err != nil || !allowed {
		t.Fatalf("seed stale attempt: allowed=%v err=%v", allowed, err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE avatar_upload_attempts SET attempted_at = now() - interval '2 hours'
		 WHERE user_id = $1 AND id = (SELECT id FROM avatar_upload_attempts WHERE user_id = $1 ORDER BY id LIMIT 1)`,
		userID,
	); err != nil {
		t.Fatalf("backdate one attempt: %v", err)
	}

	deleted, err := deleteStaleAvatarUploadAttempts(ctx, pool, time.Hour)
	if err != nil {
		t.Fatalf("deleteStaleAvatarUploadAttempts: unexpected error: %v", err)
	}
	if deleted < 1 {
		t.Errorf("deleteStaleAvatarUploadAttempts deleted %d rows, want at least 1", deleted)
	}

	var remaining int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM avatar_upload_attempts WHERE user_id = $1", userID).Scan(&remaining); err != nil {
		t.Fatalf("count avatar_upload_attempts: %v", err)
	}
	if remaining != 1 {
		t.Errorf("avatar_upload_attempts rows remaining = %d, want 1 (the recent one)", remaining)
	}
}
