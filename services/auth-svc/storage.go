package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"neobank/pkg/outbox"
)

// avatarUploadURLTTL/avatarGetURLTTL are implementation constants, not env
// config: unlike MINIO_ENDPOINT/credentials (which genuinely differ across
// environments) or the rate limit (a business knob worth tuning without a
// redeploy), these are pure mechanism details nobody needs to change
// per-deployment — same reasoning accounts-svc's resolveAttemptsRetention
// applies to its own cleanup interval.
const (
	// avatarUploadURLTTL bounds how long a presigned POST policy (from
	// POST /profile/avatar/upload-url) stays usable. Short: the whole
	// point of a presigned URL is that it authorizes exactly one upload
	// attempt for a short window, not a standing credential.
	avatarUploadURLTTL = 5 * time.Minute
	// avatarGetURLTTL bounds how long a presigned GET (from GET /profile)
	// stays usable. Long enough that a page load's <img> tags don't need
	// re-signing mid-render, short enough to limit exposure if a URL
	// leaks via a browser history entry, a proxy log, or a Referer
	// header — see README's mini-ADR on presigned-vs-public avatar
	// serving for the full reasoning.
	avatarGetURLTTL = 1 * time.Hour
)

// avatarStorage wraps the MinIO/S3 client with exactly the operations the
// avatar upload/confirm/serve/cleanup flows need. Bucket is fixed at
// construction (this system has exactly one use for object storage today);
// a second consumer would be the point to widen this into something
// bucket-parameterized, not before.
//
// Two clients, deliberately: `client` talks to MinIO over this service's
// own network path (the docker-compose internal hostname, "minio:9000")
// for every operation THIS service performs directly — Stat/Get/Put/Remove/
// List. `publicClient` is used ONLY to sign presigned URLs, and is built
// against whatever endpoint the actual requester (a browser, or a test
// driving this over host-machine HTTP) will use to reach the bucket —
// "localhost:9000" in local dev, since a docker-internal hostname like
// "minio" resolves nowhere outside the compose network. Signing a
// presigned URL with the wrong endpoint doesn't fail loudly: minio-go
// happily signs it, and the URL simply doesn't resolve for whoever tries
// to use it — this split exists because that exact mistake was caught
// live (see README's mini-ADR / manual verification section for this
// feature). In production these collapse to the same real R2/S3 endpoint,
// since nothing routes traffic to it over an internal-only hostname.
type avatarStorage struct {
	client       *minio.Client
	publicClient *minio.Client
	bucket       string
}

// newAvatarStorage constructs both MinIO clients. No network I/O happens
// here — like this service's other client constructors (Redis, Kafka),
// connecting is deferred to first use, so a MinIO that isn't up yet at
// process start doesn't fail startup.
func newAvatarStorage(endpoint, publicEndpoint, accessKey, secretKey, bucket string, useSSL bool) (*avatarStorage, error) {
	// Region is pinned explicitly (MinIO's own default) rather than left
	// for minio-go to auto-detect: auto-detection calls GetBucketLocation
	// against the client's own configured endpoint, and publicClient's
	// endpoint (see the type's doc comment) is reachable from an actual
	// external caller, not from wherever this constructor itself runs —
	// inside the auth-svc container, "localhost:9000" resolves to the
	// container's own loopback, not to MinIO. A pinned region short-
	// circuits that lookup entirely (minio-go: "Region set then no need
	// to fetch bucket location"), so presigning never needs a live
	// connection to the endpoint it signs URLs for.
	const region = "us-east-1"
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
		Region: region,
	})
	if err != nil {
		return nil, fmt.Errorf("create minio client: %w", err)
	}
	publicClient, err := minio.New(publicEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
		Region: region,
	})
	if err != nil {
		return nil, fmt.Errorf("create public-endpoint minio client: %w", err)
	}
	return &avatarStorage{client: client, publicClient: publicClient, bucket: bucket}, nil
}

// presignedUploadURL issues a presigned POST policy targeting a fresh,
// per-upload key of the form avatars/{userID}/{uuid} — never client-chosen,
// so a caller can only ever mint a target under their own prefix.
// SetContentLengthRange enforces the size ceiling AT THE STORAGE LAYER:
// MinIO/S3 itself rejects an oversized request body before it ever lands,
// which a client-side size check cannot — the client doing the checking is
// exactly what a hostile client wouldn't do.
func (s *avatarStorage) presignedUploadURL(ctx context.Context, userID string, maxBytes int64) (uploadURL string, formData map[string]string, key string, err error) {
	uploadID, err := outbox.GenerateEventID()
	if err != nil {
		return "", nil, "", fmt.Errorf("generate upload id: %w", err)
	}
	key = fmt.Sprintf("avatars/%s/%s", userID, uploadID)

	policy := minio.NewPostPolicy()
	if err := policy.SetBucket(s.bucket); err != nil {
		return "", nil, "", fmt.Errorf("set post policy bucket: %w", err)
	}
	if err := policy.SetKey(key); err != nil {
		return "", nil, "", fmt.Errorf("set post policy key: %w", err)
	}
	if err := policy.SetExpires(time.Now().UTC().Add(avatarUploadURLTTL)); err != nil {
		return "", nil, "", fmt.Errorf("set post policy expiry: %w", err)
	}
	if err := policy.SetContentLengthRange(1, maxBytes); err != nil {
		return "", nil, "", fmt.Errorf("set post policy content length range: %w", err)
	}

	u, formData, err := s.publicClient.PresignedPostPolicy(ctx, policy)
	if err != nil {
		return "", nil, "", fmt.Errorf("presigned post policy: %w", err)
	}
	return u.String(), formData, key, nil
}

// presignedGetURL signs a short-lived GET for key. Purely a local
// cryptographic computation (SigV4 signing) — no network round trip to
// MinIO, so this can't fail because MinIO happens to be slow or briefly
// unreachable, and it never confirms the object actually exists.
func (s *avatarStorage) presignedGetURL(ctx context.Context, key string) (string, error) {
	u, err := s.publicClient.PresignedGetObject(ctx, s.bucket, key, avatarGetURLTTL, nil)
	if err != nil {
		return "", fmt.Errorf("presigned get object: %w", err)
	}
	return u.String(), nil
}

// statObject reports an uploaded object's size without downloading its
// body — used to reject an oversized object cheaply before ever reading
// it into memory, on top of (not instead of) the presigned POST policy's
// own content-length-range enforcement.
func (s *avatarStorage) statObject(ctx context.Context, key string) (minio.ObjectInfo, error) {
	return s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
}

// getObject downloads key's full contents.
func (s *avatarStorage) getObject(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get object: %w", err)
	}
	defer obj.Close()

	info, err := obj.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat object: %w", err)
	}
	data := make([]byte, info.Size)
	if _, err := io.ReadFull(obj, data); err != nil {
		return nil, fmt.Errorf("read object: %w", err)
	}
	return data, nil
}

// putObject uploads data to key with contentType.
func (s *avatarStorage) putObject(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return fmt.Errorf("put object %s: %w", key, err)
	}
	return nil
}

// removeObject deletes key. Callers treat failures as best-effort (logged,
// not propagated) when the deletion is cleanup rather than the operation
// the caller's own success depends on — see avatar.go's doc comments for
// which calls are which.
func (s *avatarStorage) removeObject(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

// listObjects lists every object under prefix, recursively.
func (s *avatarStorage) listObjects(ctx context.Context, prefix string) <-chan minio.ObjectInfo {
	return s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	})
}
