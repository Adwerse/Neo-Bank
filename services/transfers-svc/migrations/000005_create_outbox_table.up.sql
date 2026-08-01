-- id is a BIGSERIAL, not the gen_random_uuid() UUID every other table in
-- this service uses: the outbox relay (a future task) needs a monotonic
-- cursor to order delivery and to resume "WHERE id > last_seen" — a UUIDv4
-- carries no such order.
CREATE TABLE outbox (
    id BIGSERIAL PRIMARY KEY,
    event_id UUID NOT NULL UNIQUE,
    event_type TEXT NOT NULL,
    partition_key TEXT NOT NULL,
    payload BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ
);

-- Partial: the relay only ever scans unpublished rows, so this index stays
-- small regardless of how large the outbox's full history grows — a plain
-- (published_at, id) index would grow with every row ever published.
CREATE INDEX idx_outbox_unpublished ON outbox (published_at, id) WHERE published_at IS NULL;
