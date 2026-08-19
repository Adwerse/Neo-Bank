-- Profile attributes live on users, not a separate table or service:
-- auth-svc already owns the user entity, and a display name/avatar are
-- attributes of the user, not of any one account — see README's mini-ADR
-- "Профиль — в auth-svc, не в отдельном profile-svc".
--
-- display_name is nullable and NOT unique: namesakes are normal, and NULL
-- (not empty string) is how "no display name set" is represented — PATCH
-- /profile treats an empty-after-trim value as a reset to NULL, never as
-- a literal empty name (see profile.go's validateDisplayName).
--
-- avatar_key/avatar_updated_at are added now, ahead of the upload feature
-- itself (next sprint), so this migration and that one don't collide:
-- avatar_key is the object key in the (MinIO-backed, S3-compatible)
-- storage bucket, not a URL — computing a URL from it is a display-time
-- concern, not something to persist and have go stale if the storage
-- endpoint ever changes.
ALTER TABLE users ADD COLUMN display_name TEXT;
ALTER TABLE users ADD COLUMN avatar_key TEXT;
ALTER TABLE users ADD COLUMN avatar_updated_at TIMESTAMPTZ;
