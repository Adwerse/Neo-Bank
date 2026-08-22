package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// newTestAvatarStorage connects to the same local MinIO docker-compose.yml
// provisions, using the same skip-if-not-set signal as newTestPool: this
// repo's convention is that DATABASE_URL unset means "the full stack
// (Postgres included) isn't up for integration tests," and MinIO is part
// of that same stack.
func newTestAvatarStorage(t *testing.T) *avatarStorage {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping test that requires the full docker-compose stack (including MinIO)")
	}
	storage, err := newAvatarStorage("localhost:9000", "localhost:9000", "neobank", "neobank_dev_password", "avatars", false)
	if err != nil {
		t.Fatalf("newAvatarStorage: %v", err)
	}
	return storage
}

// uploadViaPresignedPost performs the actual multipart/form-data POST a
// browser would make against a presigned POST policy — not just calling
// minio-go's signing function and trusting it, but proving the resulting
// URL+fields genuinely authorize an upload when driven over real HTTP.
func uploadViaPresignedPost(t *testing.T, uploadURL string, fields map[string]string, fileContent []byte) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	part, err := w.CreateFormFile("file", "upload")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(fileContent); err != nil {
		t.Fatalf("write file content: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	resp, err := http.Post(uploadURL, w.FormDataContentType(), &body)
	if err != nil {
		t.Fatalf("POST presigned upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("POST presigned upload: status %d", resp.StatusCode)
	}
}

func testJPEGBytes(t *testing.T, size int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}
	return buf.Bytes()
}

// TestPresignedUploadURL_RoundTrip proves the presigned POST policy
// mechanism itself works end-to-end over real HTTP, not just that
// minio-go's signing call returns without error — this is the "does the
// storage layer actually enforce the size ceiling" claim in disguise:
// content-length-range is baked into the policy fields returned here.
func TestPresignedUploadURL_RoundTrip(t *testing.T) {
	storage := newTestAvatarStorage(t)
	ctx := context.Background()
	userID := randomUUID(t)

	uploadURL, fields, key, err := storage.presignedUploadURL(ctx, userID, maxAvatarUploadBytes)
	if err != nil {
		t.Fatalf("presignedUploadURL: %v", err)
	}
	if !avatarKeyOwnedBy(key, userID) {
		t.Fatalf("generated key %q is not shaped as owned by %q", key, userID)
	}
	t.Cleanup(func() { _ = storage.removeObject(ctx, key) })

	uploadViaPresignedPost(t, uploadURL, fields, testJPEGBytes(t, 100))

	info, err := storage.statObject(ctx, key)
	if err != nil {
		t.Fatalf("statObject after upload: %v", err)
	}
	if info.Size == 0 {
		t.Error("uploaded object has size 0")
	}
}

// TestPresignedUploadURL_RejectsOversizedUpload confirms the storage
// layer itself — not just confirm's later StatObject check — refuses an
// upload that violates the presigned policy's content-length-range.
func TestPresignedUploadURL_RejectsOversizedUpload(t *testing.T) {
	storage := newTestAvatarStorage(t)
	ctx := context.Background()
	userID := randomUUID(t)

	// A tiny max, so a real (small) test payload still counts as "too big"
	// without actually allocating maxAvatarUploadBytes+1 of memory.
	const tinyMax = 10
	uploadURL, fields, key, err := storage.presignedUploadURL(ctx, userID, tinyMax)
	if err != nil {
		t.Fatalf("presignedUploadURL: %v", err)
	}
	t.Cleanup(func() { _ = storage.removeObject(ctx, key) })

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		w.WriteField(k, v)
	}
	part, _ := w.CreateFormFile("file", "upload")
	part.Write(bytes.Repeat([]byte("x"), tinyMax+1))
	w.Close()

	resp, err := http.Post(uploadURL, w.FormDataContentType(), &body)
	if err != nil {
		t.Fatalf("POST presigned upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		t.Errorf("oversized upload succeeded with status %d, want a rejection (content-length-range should have blocked it)", resp.StatusCode)
	}
}

// TestConfirmAvatarHandler_FullFlow is the DoD's core claim: a real JPEG
// upload goes all the way through — presigned upload, confirm, both sizes
// end up in storage and are reachable via the presigned GET URLs the
// response carries.
func TestConfirmAvatarHandler_FullFlow(t *testing.T) {
	pool := newTestPool(t)
	storage := newTestAvatarStorage(t)
	ctx := context.Background()
	userID := insertTestUser(t, ctx, pool)

	uploadURL, fields, key, err := storage.presignedUploadURL(ctx, userID, maxAvatarUploadBytes)
	if err != nil {
		t.Fatalf("presignedUploadURL: %v", err)
	}
	uploadViaPresignedPost(t, uploadURL, fields, testJPEGBytes(t, 500))
	t.Cleanup(func() {
		_ = storage.removeObject(ctx, key)
		_ = storage.removeObject(ctx, avatarThumbnailKey(key, avatarThumbnailSize))
		_ = storage.removeObject(ctx, avatarThumbnailKey(key, avatarStandardSize))
	})

	req := httptest.NewRequest(http.MethodPost, "/profile/avatar/confirm", bytes.NewReader(mustJSON(t, confirmAvatarRequest{Key: key})))
	req.Header.Set("X-User-Id", userID)
	rec := httptest.NewRecorder()
	confirmAvatarHandler(pool, storage)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var profile Profile
	if err := json.Unmarshal(rec.Body.Bytes(), &profile); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if profile.AvatarKey == nil || *profile.AvatarKey != key {
		t.Errorf("AvatarKey = %v, want %q", profile.AvatarKey, key)
	}
	if profile.AvatarURL64 == nil || profile.AvatarURL256 == nil {
		t.Fatal("AvatarURL64/AvatarURL256 are nil, want presigned URLs")
	}

	// The raw upload must be gone (superseded by the two processed sizes).
	if _, err := storage.statObject(ctx, key); err == nil {
		t.Error("raw upload object still exists after a successful confirm, want it deleted")
	}

	for _, size := range []int{avatarThumbnailSize, avatarStandardSize} {
		thumbKey := avatarThumbnailKey(key, size)
		data, err := storage.getObject(ctx, thumbKey)
		if err != nil {
			t.Fatalf("get thumbnail %s: %v", thumbKey, err)
		}
		cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("decode thumbnail %s: %v", thumbKey, err)
		}
		if format != "jpeg" {
			t.Errorf("thumbnail %s format = %q, want jpeg", thumbKey, format)
		}
		if cfg.Width != size || cfg.Height != size {
			t.Errorf("thumbnail %s dimensions = %dx%d, want %dx%d", thumbKey, cfg.Width, cfg.Height, size, size)
		}
	}

	// Row-level confirmation, independent of the handler's own response.
	row, found, err := getProfile(ctx, pool, userID)
	if err != nil || !found {
		t.Fatalf("getProfile: found=%v err=%v", found, err)
	}
	if row.AvatarKey == nil || *row.AvatarKey != key {
		t.Errorf("DB avatar_key = %v, want %q", row.AvatarKey, key)
	}
	if row.AvatarUpdatedAt == nil {
		t.Error("DB avatar_updated_at is nil, want a timestamp")
	}
}

// TestConfirmAvatarHandler_RejectsNonImage is the DoD's explicit
// requirement: a file with a .jpg-shaped key that isn't actually an image
// must be rejected at confirm — and left in storage for the cleanup
// sweep, not touched by confirm itself.
func TestConfirmAvatarHandler_RejectsNonImage(t *testing.T) {
	pool := newTestPool(t)
	storage := newTestAvatarStorage(t)
	ctx := context.Background()
	userID := insertTestUser(t, ctx, pool)

	uploadURL, fields, key, err := storage.presignedUploadURL(ctx, userID, maxAvatarUploadBytes)
	if err != nil {
		t.Fatalf("presignedUploadURL: %v", err)
	}
	// A file that would very plausibly be named avatar.jpg by whoever
	// uploaded it, but is just text — no magic bytes of any image format.
	uploadViaPresignedPost(t, uploadURL, fields, []byte("definitely not a real jpeg, just pretending to be one"))
	t.Cleanup(func() { _ = storage.removeObject(ctx, key) })

	req := httptest.NewRequest(http.MethodPost, "/profile/avatar/confirm", bytes.NewReader(mustJSON(t, confirmAvatarRequest{Key: key})))
	req.Header.Set("X-User-Id", userID)
	rec := httptest.NewRecorder()
	confirmAvatarHandler(pool, storage)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}

	// Left in place for the cleanup sweep, not deleted inline.
	if _, err := storage.statObject(ctx, key); err != nil {
		t.Errorf("rejected upload was removed from storage, want it left for the cleanup sweep: %v", err)
	}

	row, found, err := getProfile(ctx, pool, userID)
	if err != nil || !found {
		t.Fatalf("getProfile: found=%v err=%v", found, err)
	}
	if row.AvatarKey != nil {
		t.Errorf("avatar_key = %v, want nil (a rejected upload must not update the profile)", *row.AvatarKey)
	}
}

// TestConfirmAvatarHandler_ReplacementDeletesPreviousObject is task item
// 5: confirming a second avatar must delete the first one's objects, or
// storage leaks with every avatar change.
func TestConfirmAvatarHandler_ReplacementDeletesPreviousObject(t *testing.T) {
	pool := newTestPool(t)
	storage := newTestAvatarStorage(t)
	ctx := context.Background()
	userID := insertTestUser(t, ctx, pool)

	confirmOne := func(size int) string {
		uploadURL, fields, key, err := storage.presignedUploadURL(ctx, userID, maxAvatarUploadBytes)
		if err != nil {
			t.Fatalf("presignedUploadURL: %v", err)
		}
		uploadViaPresignedPost(t, uploadURL, fields, testJPEGBytes(t, size))
		req := httptest.NewRequest(http.MethodPost, "/profile/avatar/confirm", bytes.NewReader(mustJSON(t, confirmAvatarRequest{Key: key})))
		req.Header.Set("X-User-Id", userID)
		rec := httptest.NewRecorder()
		confirmAvatarHandler(pool, storage)(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("confirm status = %d, want 200; body: %s", rec.Code, rec.Body.String())
		}
		return key
	}

	firstKey := confirmOne(300)
	t.Cleanup(func() {
		_ = storage.removeObject(ctx, avatarThumbnailKey(firstKey, avatarThumbnailSize))
		_ = storage.removeObject(ctx, avatarThumbnailKey(firstKey, avatarStandardSize))
	})
	// Confirm the first avatar's objects genuinely exist before replacing them.
	if _, err := storage.statObject(ctx, avatarThumbnailKey(firstKey, avatarThumbnailSize)); err != nil {
		t.Fatalf("first avatar's thumbnail missing before replacement: %v", err)
	}

	secondKey := confirmOne(300)
	t.Cleanup(func() {
		_ = storage.removeObject(ctx, avatarThumbnailKey(secondKey, avatarThumbnailSize))
		_ = storage.removeObject(ctx, avatarThumbnailKey(secondKey, avatarStandardSize))
	})

	for _, size := range []int{avatarThumbnailSize, avatarStandardSize} {
		if _, err := storage.statObject(ctx, avatarThumbnailKey(firstKey, size)); err == nil {
			t.Errorf("first avatar's %dpx object still exists after replacement, want it deleted", size)
		}
		if _, err := storage.statObject(ctx, avatarThumbnailKey(secondKey, size)); err != nil {
			t.Errorf("second avatar's %dpx object missing: %v", size, err)
		}
	}
}

// TestCleanupStalePendingAvatars_DeletesOnlyPendingNotThumbnails uses a
// retention of 0 (everything not yet processed counts as "stale
// immediately") rather than waiting real hours or trying to backdate an
// object's server-assigned LastModified — this tests the sweep's KEY
// SHAPE filter directly: a bare pending key must be swept, a processed
// thumbnail key must never be, regardless of age.
func TestCleanupStalePendingAvatars_DeletesOnlyPendingNotThumbnails(t *testing.T) {
	storage := newTestAvatarStorage(t)
	ctx := context.Background()
	userID := randomUUID(t)
	pendingKey := fmt.Sprintf("avatars/%s/%s", userID, randomUUID(t))
	thumbnailKey := avatarThumbnailKey(pendingKey, avatarThumbnailSize)

	if err := storage.putObject(ctx, pendingKey, []byte("pending upload bytes"), "application/octet-stream"); err != nil {
		t.Fatalf("put pending object: %v", err)
	}
	if err := storage.putObject(ctx, thumbnailKey, []byte("processed thumbnail bytes"), "image/jpeg"); err != nil {
		t.Fatalf("put thumbnail object: %v", err)
	}
	t.Cleanup(func() {
		_ = storage.removeObject(ctx, pendingKey)
		_ = storage.removeObject(ctx, thumbnailKey)
	})

	deleted, err := cleanupStalePendingAvatars(ctx, storage, 0)
	if err != nil {
		t.Fatalf("cleanupStalePendingAvatars: %v", err)
	}
	if deleted < 1 {
		t.Error("cleanupStalePendingAvatars deleted 0 objects, want at least the pending one")
	}

	if _, err := storage.statObject(ctx, pendingKey); err == nil {
		t.Error("pending upload object still exists after cleanup, want it deleted")
	}
	if _, err := storage.statObject(ctx, thumbnailKey); err != nil {
		t.Errorf("thumbnail object was deleted by cleanup, want it left alone: %v", err)
	}
}

// TestCleanupStalePendingAvatars_RespectsRetention confirms a pending
// upload NEWER than retention is left alone — the sweep must not delete
// an upload a confirm request might still be in the middle of processing.
func TestCleanupStalePendingAvatars_RespectsRetention(t *testing.T) {
	storage := newTestAvatarStorage(t)
	ctx := context.Background()
	userID := randomUUID(t)
	pendingKey := fmt.Sprintf("avatars/%s/%s", userID, randomUUID(t))

	if err := storage.putObject(ctx, pendingKey, []byte("freshly uploaded"), "application/octet-stream"); err != nil {
		t.Fatalf("put pending object: %v", err)
	}
	t.Cleanup(func() { _ = storage.removeObject(ctx, pendingKey) })

	if _, err := cleanupStalePendingAvatars(ctx, storage, 24*time.Hour); err != nil {
		t.Fatalf("cleanupStalePendingAvatars: %v", err)
	}

	if _, err := storage.statObject(ctx, pendingKey); err != nil {
		t.Errorf("fresh pending object was deleted despite a 24h retention: %v", err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	return data
}
