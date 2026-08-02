-- notifications_processed_events, not "processed_events": every service
-- shares one physical "neobank" Postgres database, and accounts-svc
-- already owns a table literally named "processed_events" — a bare name
-- here would collide (the same class of collision transfers-svc's
-- "outbox" table already forced auth-svc/accounts-svc to dodge with
-- "auth_outbox"/"accounts_outbox").
--
-- Idempotent-consumer dedup table, same purpose as accounts-svc's own —
-- shared by both UserActivated and AccountCreated processing (event_id
-- is a globally unique UUID regardless of which event type it belongs
-- to, so one table for both is fine).
--
-- status exists because notifications-svc will eventually decide whether
-- processing an event results in actually sending a notification
-- ('sent'), deciding not to ('skipped'), or is mid-flight ('processing').
-- This sprint only ever writes 'skipped' — see kafka.go — since sending
-- notifications is explicitly out of scope here; the column is part of
-- this migration now so a later sprint doesn't need a schema change to
-- start using the other two values.
CREATE TABLE notifications_processed_events (
    event_id UUID PRIMARY KEY,
    status TEXT NOT NULL CHECK (status IN ('processing', 'sent', 'skipped')),
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
