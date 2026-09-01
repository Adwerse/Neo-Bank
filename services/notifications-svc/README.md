# notifications-svc

A pure consumer — no HTTP API of its own beyond `/healthz`. Turns Kafka
events into transactional email through Mailpit, with retry, a
dead-letter topic, and an idempotency barrier so a redelivered event never
sends a duplicate silently.

**Owns:** `user_contacts` (its own local projection of `user_id`/`account_id`
→ `email`, built entirely from events — no synchronous calls to auth-svc or
accounts-svc, ever) and `notifications_processed_events`, the
`processing`→`sent` idempotency barrier per event.

**API:** none — `GET /healthz` only, which also reports Kafka broker
reachability and per-topic consumer lag, not just "the process is alive."

**Consumes:** `user.events`/`account.events` (build the contact projection)
and `transfer.events` (`TransferCompleted`/`Failed`/`Rejected`,
`DepositCredited` → up to two emails per transfer, retried up to 5 times
with backoff, then routed to `transfer.events.dlq`).

**Talks to:** Kafka, its own Postgres schema, Mailpit (SMTP) — no other
service.

Full reasoning — why a fraud-block email never names the rule, the
`event_type` header wire contract, graceful shutdown mid-retry: root
[README](../../README.md).
