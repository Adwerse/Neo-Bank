// Avatar upload follows the same "bytes never cross the backend" principle
// as Stripe Elements elsewhere in this system: POST /profile/avatar/upload-url
// hands the client a presigned target and this service never touches the
// request body of the actual upload. The price of that principle is that
// nothing can be validated before the bytes land in storage — validation
// happens entirely at POST /profile/avatar/confirm, against whatever is
// already sitting in the bucket.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png" // registers the PNG decoder with image.Decode/DecodeConfig; this service never encodes PNG
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	xdraw "golang.org/x/image/draw"
)

const (
	// maxAvatarUploadBytes is enforced twice: at the storage layer via the
	// presigned POST policy's content-length-range (storage.go), and again
	// here via StatObject before ever downloading the body — belt and
	// braces, since the first is what actually stops an oversized upload
	// from landing at all, and the second is what protects this service
	// even if that policy were ever misconfigured or bypassed.
	maxAvatarUploadBytes = 8 << 20 // 8 MiB
	// maxAvatarPixels defends against decompression bombs: a small file
	// (kilobytes) whose declared dimensions decode to a huge pixel grid,
	// exhausting memory on Decode. Checked against image.DecodeConfig's
	// header-only read, before the expensive full Decode ever runs.
	maxAvatarPixels = 20_000_000 // 20 MP — a generous ceiling for a phone photo, a hostile one for a bomb

	avatarThumbnailSize = 64
	avatarStandardSize  = 256
	avatarJPEGQuality   = 85
)

// allowedAvatarContentTypes is the explicit, closed list of formats this
// endpoint accepts — checked against what http.DetectContentType sniffs
// from the actual downloaded bytes, never against the upload's file
// extension or the client's declared Content-Type. Both of those are
// trivially forgeable by whoever controls the upload; only the content
// itself cannot be.
var allowedAvatarContentTypes = map[string]struct{}{
	"image/jpeg": {},
	"image/png":  {},
}

// avatarKeyOwnedBy reports whether key is exactly the shape this service
// itself generates for userID via avatarStorage.presignedUploadURL:
// "avatars/{userID}/{uploadID}", no more and no fewer path segments. This
// is the one authorization check standing between "confirm my own upload"
// and "confirm an arbitrary storage key" — a key under a different user's
// prefix, or one with extra segments (e.g. an already-processed
// "avatars/{user}/{id}/64" thumbnail), is rejected outright before this
// service ever asks the storage layer about it.
func avatarKeyOwnedBy(key, userID string) bool {
	parts := strings.Split(key, "/")
	return len(parts) == 3 && parts[0] == "avatars" && parts[1] == userID && parts[2] != ""
}

// avatarThumbnailKey derives a processed size's object key from the
// pending upload's base key — "avatars/{user}/{id}" becomes
// "avatars/{user}/{id}/64" or "/256".
func avatarThumbnailKey(base string, size int) string {
	return fmt.Sprintf("%s/%d", base, size)
}

// decodeAvatarImage validates data is really an allowed image (by content,
// not extension or client-declared Content-Type) and within safe
// size/resolution bounds, then fully decodes it. Order is deliberate: the
// cheap checks (byte length, sniffed type, DecodeConfig's header-only
// read) all happen before the expensive one (full Decode, which allocates
// the whole pixel buffer) — a hostile or merely corrupt upload never pays
// for more than it needs to reject.
func decodeAvatarImage(data []byte) (image.Image, error) {
	if len(data) > maxAvatarUploadBytes {
		return nil, fmt.Errorf("image is %d bytes, want %d or fewer", len(data), maxAvatarUploadBytes)
	}

	sniffLen := len(data)
	if sniffLen > 512 { // http.DetectContentType only ever inspects the first 512 bytes itself
		sniffLen = 512
	}
	contentType := http.DetectContentType(data[:sniffLen])
	if _, ok := allowedAvatarContentTypes[contentType]; !ok {
		return nil, fmt.Errorf("unsupported image type %q", contentType)
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode image header: %w", err)
	}
	if pixels := cfg.Width * cfg.Height; pixels > maxAvatarPixels {
		return nil, fmt.Errorf("image is %dx%d (%d pixels), want %d or fewer", cfg.Width, cfg.Height, pixels, maxAvatarPixels)
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	return img, nil
}

// cropAndResize center-crops img to a square (side = the shorter
// dimension) and scales that crop to size x size. This is also where EXIF
// and every other embedded metadata chunk disappears: img is already
// decoded pixel data with no memory of the source file's byte layout, and
// encodeJPEG below writes a brand new file from scratch — there is no
// GPS tag, no polyglot payload, nothing left over from the original bytes
// to carry forward, because nothing here ever touches those bytes again.
func cropAndResize(img image.Image, size int) image.Image {
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	side := w
	if h < side {
		side = h
	}
	left := bounds.Min.X + (w-side)/2
	top := bounds.Min.Y + (h-side)/2
	srcRect := image.Rect(left, top, left+side, top+side)

	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	// White background first: a source PNG's transparent pixels would
	// otherwise composite onto (premultiplied) black, and JPEG — this
	// function's eventual output format — has no alpha channel to
	// preserve transparency with anyway.
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, srcRect, xdraw.Over, nil)
	return dst
}

func encodeJPEG(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type uploadAvatarURLResponse struct {
	URL    string            `json:"url"`
	Fields map[string]string `json:"fields"`
	Key    string            `json:"key"`
}

// uploadAvatarURLHandler is POST /profile/avatar/upload-url. Rate-limited
// per user (recordAvatarUploadAttempt) — issuing a presigned URL costs
// this service nothing, so without a limit here, the limit-free part of
// the flow becomes a way to mint unbounded upload targets and fill the
// bucket, regardless of what confirm later rejects.
func uploadAvatarURLHandler(pool *pgxpool.Pool, storage *avatarStorage, rateLimit int, rateWindow time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		userID := r.Header.Get("X-User-Id")
		if userID == "" {
			writeJSONError(w, http.StatusBadRequest, "missing X-User-Id header")
			return
		}

		allowed, err := recordAvatarUploadAttempt(r.Context(), pool, userID, rateLimit, rateWindow)
		if err != nil {
			log.Printf("auth-svc: uploadAvatarURL: rate limit check: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		if !allowed {
			writeJSONError(w, http.StatusTooManyRequests, "too many avatar upload requests, try again later")
			return
		}

		uploadURL, fields, key, err := storage.presignedUploadURL(r.Context(), userID, maxAvatarUploadBytes)
		if err != nil {
			log.Printf("auth-svc: uploadAvatarURL: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(uploadAvatarURLResponse{URL: uploadURL, Fields: fields, Key: key})
	}
}

type confirmAvatarRequest struct {
	Key string `json:"key"`
}

// confirmAvatarHandler is POST /profile/avatar/confirm. See this file's
// package doc comment for why validation only happens here rather than at
// upload time. A rejected upload's object is deliberately left in
// storage — see runAvatarCleanupWorker (avatar_cleanup.go) — rather than
// deleted inline, so a confirm that fails partway through never has to
// reason about whether it's safe to delete something a retry might still
// need.
func confirmAvatarHandler(pool *pgxpool.Pool, storage *avatarStorage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		userID := r.Header.Get("X-User-Id")
		if userID == "" {
			writeJSONError(w, http.StatusBadRequest, "missing X-User-Id header")
			return
		}

		var req confirmAvatarRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if !avatarKeyOwnedBy(req.Key, userID) {
			writeJSONError(w, http.StatusBadRequest, "invalid key")
			return
		}

		ctx := r.Context()

		info, err := storage.statObject(ctx, req.Key)
		if err != nil {
			writeJSONError(w, http.StatusNotFound, "avatar upload not found")
			return
		}
		if info.Size > maxAvatarUploadBytes {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("image is %d bytes, want %d or fewer", info.Size, maxAvatarUploadBytes))
			return
		}

		data, err := storage.getObject(ctx, req.Key)
		if err != nil {
			log.Printf("auth-svc: confirmAvatar: get object %s: %v", req.Key, err)
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}

		img, err := decodeAvatarImage(data)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}

		thumb64, err := encodeJPEG(cropAndResize(img, avatarThumbnailSize), avatarJPEGQuality)
		if err != nil {
			log.Printf("auth-svc: confirmAvatar: encode %dpx: %v", avatarThumbnailSize, err)
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		thumb256, err := encodeJPEG(cropAndResize(img, avatarStandardSize), avatarJPEGQuality)
		if err != nil {
			log.Printf("auth-svc: confirmAvatar: encode %dpx: %v", avatarStandardSize, err)
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}

		if err := storage.putObject(ctx, avatarThumbnailKey(req.Key, avatarThumbnailSize), thumb64, "image/jpeg"); err != nil {
			log.Printf("auth-svc: confirmAvatar: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		if err := storage.putObject(ctx, avatarThumbnailKey(req.Key, avatarStandardSize), thumb256, "image/jpeg"); err != nil {
			log.Printf("auth-svc: confirmAvatar: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}

		// The raw upload is never served — see the package doc comment on
		// why nothing downstream may ever read the original bytes again.
		// Both processed sizes exist now, so it's redundant; best-effort,
		// since a failure here just means the cleanup sweep reclaims it.
		if err := storage.removeObject(ctx, req.Key); err != nil {
			log.Printf("auth-svc: confirmAvatar: remove raw upload %s: %v", req.Key, err)
		}

		oldKey, found, err := swapAvatarKey(ctx, pool, userID, req.Key)
		if err != nil {
			log.Printf("auth-svc: confirmAvatar: swap avatar_key: %v", err)
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		if !found {
			writeJSONError(w, http.StatusNotFound, "user not found")
			return
		}
		if oldKey != nil && *oldKey != req.Key {
			// Best-effort: an orphaned old thumbnail is a storage-hygiene
			// issue, not a reason to fail a confirm that already
			// succeeded — see task item 5, "replacing must delete the
			// previous object."
			if err := storage.removeObject(ctx, avatarThumbnailKey(*oldKey, avatarThumbnailSize)); err != nil {
				log.Printf("auth-svc: confirmAvatar: remove old thumbnail: %v", err)
			}
			if err := storage.removeObject(ctx, avatarThumbnailKey(*oldKey, avatarStandardSize)); err != nil {
				log.Printf("auth-svc: confirmAvatar: remove old thumbnail: %v", err)
			}
		}

		profile, found, err := getProfile(ctx, pool, userID)
		if err != nil || !found {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(withAvatarURLs(ctx, storage, profile))
	}
}

// swapAvatarKey sets the user's avatar_key/avatar_updated_at to newKey and
// returns whatever avatar_key was there immediately before — the caller
// needs the OLD value to delete its now-superseded thumbnail objects.
// SELECT ... FOR UPDATE then UPDATE within one transaction, not an UPDATE
// ... RETURNING (SELECT ...): a RETURNING subquery would see the row
// AFTER this same statement's own write, not before it.
func swapAvatarKey(ctx context.Context, pool *pgxpool.Pool, userID, newKey string) (oldKey *string, found bool, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)

	err = tx.QueryRow(ctx, "SELECT avatar_key FROM users WHERE id = $1 FOR UPDATE", userID).Scan(&oldKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	if _, err := tx.Exec(ctx, "UPDATE users SET avatar_key = $1, avatar_updated_at = now() WHERE id = $2", newKey, userID); err != nil {
		return nil, false, err
	}

	return oldKey, true, tx.Commit(ctx)
}

// withAvatarURLs returns profile with AvatarURL64/AvatarURL256 populated
// from AvatarKey, if set. presignedGetURL is a local signing operation
// (see storage.go) — it cannot fail because MinIO happens to be slow or
// briefly unreachable, so a failure here is truly exceptional and logged
// rather than turned into a failed response for what is, from the
// caller's perspective, a successful profile fetch.
func withAvatarURLs(ctx context.Context, storage *avatarStorage, profile Profile) Profile {
	if profile.AvatarKey == nil {
		return profile
	}
	if url64, err := storage.presignedGetURL(ctx, avatarThumbnailKey(*profile.AvatarKey, avatarThumbnailSize)); err != nil {
		log.Printf("auth-svc: presigned avatar url (%dpx): %v", avatarThumbnailSize, err)
	} else {
		profile.AvatarURL64 = &url64
	}
	if url256, err := storage.presignedGetURL(ctx, avatarThumbnailKey(*profile.AvatarKey, avatarStandardSize)); err != nil {
		log.Printf("auth-svc: presigned avatar url (%dpx): %v", avatarStandardSize, err)
	} else {
		profile.AvatarURL256 = &url256
	}
	return profile
}
