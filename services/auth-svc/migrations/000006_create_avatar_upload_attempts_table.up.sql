-- Backs the per-user rate limit on issuing avatar upload URLs
-- (avatar_rate_limit.go). Without this, POST /profile/avatar/upload-url
-- is a way to mint an unbounded number of presigned PUT targets and fill
-- the storage bucket with garbage — same reasoning as accounts-svc's
-- iban_resolve_attempts (migration 000006 there), same table shape.
CREATE TABLE avatar_upload_attempts (
    id BIGSERIAL PRIMARY KEY,
    user_id UUID NOT NULL,
    attempted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Serves the windowed COUNT in recordAvatarUploadAttempt (WHERE user_id =
-- $1 AND attempted_at > ...). The periodic cleanup sweep is an
-- unqualified DELETE ... WHERE attempted_at < ... and does not use this
-- index — same tradeoff as iban_resolve_attempts's identical index.
CREATE INDEX idx_avatar_upload_attempts_user_window ON avatar_upload_attempts (user_id, attempted_at);
