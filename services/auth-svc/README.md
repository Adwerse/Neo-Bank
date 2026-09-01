# auth-svc

Owns the user: credentials, sessions, and profile. The only service with a
name field of any kind — nobody else stores one.

**Owns:** `users` (email, password hash, status), `verification_codes`,
refresh-token state in Redis, `display_name`/`avatar_key` on the user row,
and the `avatars` bucket in MinIO (via presigned URLs — bytes never pass
through this service).

**API:** `POST /register`, `/verify-email`, `/login`, `/refresh`, `/logout`,
`GET`/`PATCH /profile`, `POST /profile/avatar/upload-url`,
`POST /profile/avatar/confirm`.

**Publishes:** `UserActivated` (on email verification) and `ProfileUpdated`
(on a display-name change) to `user.events`, through the same transactional
outbox as every other producer in this repo — never a direct Kafka write.

**Talks to:** its own Postgres schema, Redis (refresh tokens, rate limits),
MinIO (avatar objects). Calls no other service.

Full reasoning — avatar validation pipeline, token-storage trade-off,
`display_name` spoofing checks: root [README](../../README.md).
