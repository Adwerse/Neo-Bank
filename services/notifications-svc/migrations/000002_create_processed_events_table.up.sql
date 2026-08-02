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
-- status records whether processing an event resulted in actually
-- sending a notification ('sent'), deliberately not sending one
-- ('skipped'), or is mid-flight ('processing'). The column was added
-- here, ahead of need, so that the sprint which turned on email sending
-- wouldn't require a schema change — and it now uses all three:
--   processing — claimed by claimEvent, side effects in flight (or in
--                flight when the process died, which is the honest
--                meaning of a row left in this state)
--   sent       — at least one email was accepted by the SMTP server
--   skipped    — no email by design: the projection-only events
--                (UserActivated, AccountCreated), or a transfer event
--                whose parties resolve to nobody we hold an address for
-- See services/notifications-svc/contacts.go (claimEvent/finishEvent)
-- and README's «Барьер идемпотентности» for why 'processing' is treated
-- as "retry" rather than "skip".
CREATE TABLE notifications_processed_events (
    event_id UUID PRIMARY KEY,
    status TEXT NOT NULL CHECK (status IN ('processing', 'sent', 'skipped')),
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
