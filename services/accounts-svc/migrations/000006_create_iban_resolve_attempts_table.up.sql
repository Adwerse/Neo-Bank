-- Backs the per-user rate limit on ResolveAccountByIban (grpc_server.go).
-- A resolve endpoint that answers "exists"/"doesn't exist" for a
-- checksum-valid IBAN is an enumeration oracle: valid check digits narrow
-- the space worth guessing, they don't make guessing safe. One row per
-- allowed attempt, counted in a trailing window — see
-- recordResolveAttempt in rate_limit.go for the query that reads and
-- writes this table atomically.
CREATE TABLE iban_resolve_attempts (
    id BIGSERIAL PRIMARY KEY,
    user_id UUID NOT NULL,
    attempted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Serves recordResolveAttempt's windowed COUNT (WHERE user_id = $1 AND
-- attempted_at > ...). The periodic cleanup sweep
-- (runResolveAttemptsCleanupWorker) is an unqualified DELETE ... WHERE
-- attempted_at < ... and does not use this index — an occasional full scan
-- of a table this small and bounded is cheaper than maintaining a second
-- index for a worker that runs once an hour.
CREATE INDEX idx_iban_resolve_attempts_user_window ON iban_resolve_attempts (user_id, attempted_at);
