package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// avatarCleanupInterval/Retention govern the sweep for orphaned pending
// uploads: an object sitting at the bare "avatars/{user}/{id}" key (no
// "/64" or "/256" suffix) that confirm never finished processing — either
// the client never called confirm at all, or confirm rejected the upload
// during validation (see decodeAvatarImage) and deliberately left it in
// place rather than deleting it inline. A successful confirm always
// deletes its own bare key (confirmAvatarHandler, avatar.go), so anything
// still there past retention is presumed abandoned.
const (
	avatarCleanupInterval  = 1 * time.Hour
	avatarCleanupRetention = 24 * time.Hour
)

// runAvatarCleanupWorker periodically sweeps stale pending avatar uploads
// out of storage. Same ticker-loop shape as
// runAvatarUploadAttemptsCleanupWorker (avatar_rate_limit.go) and
// accounts-svc's runResolveAttemptsCleanupWorker — a different resource
// (object storage, not a Postgres table) but the identical "periodic,
// log-and-continue-on-error, log-only-if-anything-was-deleted" shape.
func runAvatarCleanupWorker(ctx context.Context, storage *avatarStorage) {
	ticker := time.NewTicker(avatarCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := cleanupStalePendingAvatars(ctx, storage, avatarCleanupRetention)
			if err != nil {
				log.Printf("auth-svc: pending avatar cleanup: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("auth-svc: pending avatar cleanup: deleted %d stale upload(s)", n)
			}
		}
	}
}

// cleanupStalePendingAvatars lists every object under "avatars/" and
// deletes the ones shaped like a pending upload — exactly
// "avatars/{user}/{id}" (two slashes), never a processed
// "avatars/{user}/{id}/{size}" thumbnail (three slashes), which this sweep
// must never touch — whose LastModified is older than retention.
func cleanupStalePendingAvatars(ctx context.Context, storage *avatarStorage, retention time.Duration) (int, error) {
	cutoff := time.Now().Add(-retention)
	deleted := 0
	for obj := range storage.listObjects(ctx, "avatars/") {
		if obj.Err != nil {
			return deleted, fmt.Errorf("list avatar objects: %w", obj.Err)
		}
		if strings.Count(obj.Key, "/") != 2 {
			continue // a processed thumbnail, not a pending upload
		}
		if obj.LastModified.After(cutoff) {
			continue // not stale yet
		}
		if err := storage.removeObject(ctx, obj.Key); err != nil {
			log.Printf("auth-svc: pending avatar cleanup: remove %s: %v", obj.Key, err)
			continue
		}
		deleted++
	}
	return deleted, nil
}
