# Devlog

Chronological record of work done on the Neo-Bank monorepo, one entry per commit.

## 2026-07-06 — `664448c` — Initial repo scaffold
- Created the monorepo skeleton: `gateway/`, `services/{auth,accounts,ledger,transfers,fraud,notifications}-svc/`, `proto/`, `.github/workflows/` — empty directories, no code yet.
- Added `docker-compose.yml` describing all 7 services (build path + port each, no top-level `version:` key since it's deprecated in modern Compose): gateway 8080, auth-svc 8081, accounts-svc 8082, ledger-svc 8083, transfers-svc 8084, fraud-svc 8085, notifications-svc 8086.
- Added `README.md` describing the structure and next steps.
- Verified: `docker compose config` parses and validates cleanly even with no Dockerfiles present yet.

## 2026-07-06 — `d257124` — Minimal Go code + Dockerfiles for all 7 services
- Every service and the gateway got a working `go.mod` (`module neobank/...`, `go 1.22`) and `main.go`: a stdlib-only HTTP server that reads `PORT` from the environment (falling back to its assigned port) and answers `/` with `501 Not Implemented` + `{"service":"<name>"}` — just enough to prove the container is alive.
- Added a multi-stage `Dockerfile` per service: `golang:1.22-alpine` build stage → `alpine:latest` runtime stage, copying only the compiled binary.
- Verified: `docker compose build` succeeded for all 7 images; `docker compose up` brought up all 7 containers with no restart loop; curled every port and got the expected `501` JSON body.

## 2026-07-06 — `b88b4ff` — Shared `/healthz` endpoint via a common Go package
- Added `GET /healthz` → `200 {"status":"ok","service":"<name>"}` to every service, without copy-pasting the handler 7 times.
- Extracted the handler into a new local Go module, `pkg/health` (`neobank/pkg/health`), wired into every service's `go.mod` via `require` + `replace ... => ../../pkg/health` — the standard way to share code between independent Go modules in a monorepo without a module registry.
- That forced a build-context change: since each service is its own Go module *and* its own Docker build context, and `pkg/health` lives outside every service's folder, every Dockerfile moved to a repo-root build context (`context: .` + `dockerfile: <path>/Dockerfile` in `docker-compose.yml`) so it could `COPY pkg/health` alongside the service's own folder.
- Named the package `pkg/health` rather than `internal/health` as originally suggested — `internal` triggers Go's import-visibility rule; it generally still works across modules via `replace`, but there's no upside in a private repo, so the risk wasn't worth taking.
- Verified: `docker compose build` + `up` all green; curled `/healthz` on all 7 ports (200 + correct service name) and confirmed `/` still returns `501` unchanged.

## 2026-07-07 — `4a0fb44` — Dev infrastructure: Postgres, Redis, Kafka
- Added to `docker-compose.yml`: `postgres:16-alpine` (named volume for persistence, dev credentials via env vars, `pg_isready` healthcheck), `redis:7-alpine` (`redis-cli ping` healthcheck), and Kafka running single-node KRaft (no Zookeeper). None of the 7 services connect to this infra yet — that's scoped to future per-service work (ledger-svc, transfers-svc, etc.).
- Documented in `README.md` that the Postgres credentials are for local dev only, not production.
- **Deviation from plan**: the task asked for `bitnami/kafka`, but Bitnami discontinued free Docker Hub distribution for that image (moved it behind a paid "Secure Images" subscription) — confirmed via the Docker Hub API, which now returns zero tags for `bitnami/kafka`. Substituted `bitnamilegacy/kafka:3.7.1`, the frozen, still-free archive of the same image, with the same KRaft environment variables.
- Verified: all 10 containers (7 services + 3 infra) come up together; `postgres` and `redis` report `healthy` in `docker compose ps`; created and listed a test topic via `kafka-topics.sh` inside the Kafka container; Kafka logs confirm a clean KRaft startup.

## 2026-07-07 — `ee8a6fd` — Started this devlog
- Added `DEVLOG.md` itself, seeded with the three entries above, to keep a chronological, one-entry-per-commit record of the project going forward.

## 2026-07-07 — `4be3cb5` — Wired up `proto/` with buf + a Go workspace
- Added `buf.yaml`/`buf.gen.yaml` (buf config for linting and code generation) and `proto/common/v1/common.proto`, a placeholder `Empty` message with no fields — just enough to prove `buf generate` produces working Go code, not a real contract yet.
- Generated code lands in `proto/gen/go` (its own Go module, `neobank/proto/gen/go`, depending only on `google.golang.org/protobuf`) — documented in `proto/README.md` as never hand-edited, always regenerated via `buf generate`, and the convention that a service only gets a `.proto` file in the sprint it actually needs a gRPC contract, not proactively.
- Introduced `go.work` (Go workspace, `go 1.22` at the time) listing all 7 services + `gateway` + `pkg/health` + `proto/gen/go` — the mechanism that lets every independent Go module in the monorepo be built/tested together without a shared module root.
- No service consumes `proto/gen/go` yet; this is pure scaffolding for future inter-service gRPC calls.

## 2026-07-07 — `3ef964c` — CI/CD pipeline
- Added `.github/workflows/ci.yml`: on every push/PR, checks out the repo, sets up Go from `go.work`'s version, then uses `go list -m -f '{{.Dir}}'` to enumerate every workspace module and runs `go build ./... && go vet ./...` in each one's directory.
- Deliberately loops per-module rather than running a single build/vet from the repo root, so a `go vet all` doesn't surface pre-existing findings inside third-party dependencies as if they were this repo's own issues; and deliberately has no hardcoded service list, so `go.work`'s `use` block is the single source of truth for what CI builds.
- No lint step (no `golangci-lint`) and no `go test` step yet — build+vet only, matching the project's current early stage.
- Bumped `go.work` and `proto/gen/go`'s `go.mod` from `1.22` to `1.23` alongside this (setup-go needs a concrete version to install).

## 2026-07-09 — `ad7e37c` — auth-svc connects to Postgres; first migrations; mailpit added
- Gave `auth-svc` a real Postgres connection (`pgxpool.New` against a new `DATABASE_URL` env var) and rewrote `/healthz` to round-trip a real `SELECT 1` query instead of the shared `pkg/health` handler, returning `503` if the query fails.
- Added auth-svc's first two migrations (plain up/down `.sql` files, no migration tool wired in yet): `users` (`id UUID`, `email UNIQUE`, `password_hash`, `status` constrained to `pending_verification`/`active`/`suspended`, timestamps) and `verification_codes` (`user_id` FK, `purpose` constrained to `email_verify`/`password_reset`, `code_hash`, `expires_at`, `attempts_remaining`, `used_at`).
- Added `mailpit` to `docker-compose.yml` (SMTP on `1025`, web UI on `8025`) for capturing outbound email in dev without a real SMTP provider.
- **Deviation, tech debt**: this is also where auth-svc quietly diverged from every other service — it dropped the shared `pkg/health` dependency entirely (custom DB-backed healthcheck needs its own logic anyway) and bumped its own `go.mod`/Dockerfile to `golang:1.25-alpine` (pgx's toolchain requirement), while `gateway` and the other stub services stayed on `1.22`. `go.work` itself was bumped to `1.25.0` to cover it. This mixed-version state across modules is intentional-but-undocumented tech debt worth reconciling later.
- auth-svc's Dockerfile also simplified to build from its own module directory directly (`COPY services/auth-svc .`) now that it no longer needs `pkg/health` copied in alongside it.

## 2026-07-11 — `22fc31a` — `/register` endpoint
- Added `POST /register`: validates email format and an 8-character minimum password, hashes the password with **argon2id** (`hashPassword`, PHC-formatted output: `$argon2id$v=...$m=...,t=...,p=...$salt$hash`), creates the user row (`status = pending_verification`), generates a random 6-digit code, stores its SHA-256 hash in `verification_codes` (10-minute expiry, 5 attempts), and emails the plaintext code via mailpit (`net/smtp`). Returns `201 {"status":"pending_verification"}`.
- Added `SMTP_ADDR`/`SMTP_FROM` env vars (defaults `mailpit:1025`/`noreply@neobank.local`) and wired `mailpit` into auth-svc's `depends_on` (`condition: service_healthy`).

## 2026-07-11 — `ae9561f` — `/verify-email` and `/resend-verification`
- Added `POST /verify-email` (`verify.go`): looks up the user's newest unused `email_verify` code, checks expiry and remaining attempts, compares the SHA-256 hash in constant application logic (decrementing `attempts_remaining` on a wrong guess), and on success marks the code used and flips the user to `status = active`.
- Added `POST /resend-verification`: rate-limited via a Redis `SETNX` key (`resend-verification:<email>`, 60s cooldown, `429` if still cooling down), reuses the same code-issuing path as registration.
- Refactored the shared "invalidate old codes, insert a fresh one" logic out of `registerUserAndIssueCode` into `invalidateAndIssueCode`, now called by both registration and resend.
- Added the `REDIS_ADDR` env var (default `redis:6379`) and wired `redis` into auth-svc's `depends_on`.

## 2026-07-11 — `1b5083e` — `/login`, `/refresh`, `/logout` — real JWTs
- Added `POST /login` (`login.go`): verifies the password against the stored argon2id hash — always running the comparison, using a precomputed dummy hash when the email doesn't match anything, so a wrong-email and a wrong-password attempt take the same code path and roughly the same time (timing-attack mitigation) — rejects non-`active` accounts, then issues a token pair.
- Access tokens: `github.com/golang-jwt/jwt/v5`, **HS256**, signed with `JWT_SECRET` (required env var, no default — auth-svc now fails fast at startup if it's unset), 15-minute TTL, claims are just `user_id`/`email` plus standard `iat`/`exp` (no `iss`/`sub`/`aud`).
- Refresh tokens are *not* JWTs: 32 random bytes, base64url-encoded, stored in Redis as `refresh:<token>` → `userID` with a 7-day TTL. `POST /refresh` atomically pops the token (`GetDel`, so it's single-use), re-checks the user is still `active`, and issues a fresh pair. `POST /logout` just deletes the given refresh token from Redis.
- Verified live in this session (as part of documenting it): the full `register → verify-email → login → refresh/logout` cycle works end-to-end against real Postgres/Redis/mailpit containers.

## 2026-07-12 — `bf2f43a` — Gateway: reverse proxy + JWT middleware
- `gateway/` went from a `501`-stub to the project's actual entry point: `httputil.ReverseProxy` (wrapped in `http.StripPrefix`, not a hand-rolled `Director`, to avoid a `RawPath`-staleness bug with percent-encoded paths) routes by URL prefix to all 6 backend services (`/auth`→`auth-svc:8081`, `/accounts`→`accounts-svc:8082`, `/ledger`→`ledger-svc:8083`, `/transfers`→`transfers-svc:8084`, `/fraud`→`fraud-svc:8085`, `/notifications`→`notifications-svc:8086`), each target overridable via a `*_SVC_ADDR` env var.
- Added a JWT middleware (`gateway/middleware.go`) wrapping the entire router: parses `Authorization: Bearer <token>`, validates signature + expiry with `jwt.ParseWithClaims` + `jwt.WithValidMethods([]string{"HS256"})` (blocks algorithm-confusion attacks) against the **same `JWT_SECRET`** auth-svc signs with. Fail-secure by default — every path needs a valid token unless it's in an explicit allowlist: `/`, `/healthz`, `/auth/register`, `/auth/verify-email`, `/auth/resend-verification`, `/auth/login`, `/auth/refresh`, `/auth/forgot-password`, `/auth/reset-password` (the last two reserved ahead of their actual implementation). Notably `/auth/logout` is *not* allowlisted, so it now requires a token even though it lives under `/auth`.
- Explicitly out of scope for this pass: rate limiting and dynamic service discovery — the routing table is a small static list, matching how early-stage the other 5 services still are (all still `501` stubs behind their gateway routes).
- Added `github.com/golang-jwt/jwt/v5` (same pinned version as auth-svc, `v5.3.1`) to `gateway/go.mod`; wired `JWT_SECRET` and `depends_on: auth-svc` into the `gateway` service in `docker-compose.yml`.
- Verified live: through the gateway (port `8080`), `register` → `verify-email` → `login` all succeed with no `Authorization` header; an unauthenticated call to `/auth/logout` returns `401`; the same call with the access token from `/login` passes through to auth-svc and returns `200`; `/accounts/` (no backend logic yet) correctly returns `401` unauthenticated and proxies through to accounts-svc's `501` stub once authenticated, confirming the routing table generalizes beyond `/auth`. Also caught and fixed a duplicate `Content-Type` header bug during this verification: the middleware was setting the header before handing off to the proxy, which then merged with auth-svc's own `Content-Type` header on success responses.

## Current structure (as of 2026-07-12)

**Runtime topology** — 6 Go services + gateway, all `net/http` stdlib-only (no router library anywhere in the repo), one Go workspace (`go.work`, mixed `go 1.22`/`1.25.0` across modules — see the `ad7e37c` deviation note above), built as independent Docker images from a shared repo-root build context:

| Service | Port | State |
|---|---|---|
| `gateway` | 8080 | Real: reverse proxy + JWT auth in front of everything below |
| `auth-svc` | 8081 | Real: register/verify-email/resend-verification/login/refresh/logout, Postgres + Redis + mailpit backed |
| `accounts-svc` | 8082 | Stub (`501`) |
| `ledger-svc` | 8083 | Stub (`501`) |
| `transfers-svc` | 8084 | Stub (`501`) |
| `fraud-svc` | 8085 | Stub (`501`) |
| `notifications-svc` | 8086 | Stub (`501`) |

**Dev infra** (`docker-compose.yml`): `postgres:16-alpine` (db `neobank`, 2 migrations so far: `users`, `verification_codes` — only consumed by auth-svc), `redis:7-alpine` (refresh tokens + resend-verification rate limiting, only consumed by auth-svc), `mailpit` (fake SMTP + web UI on `8025`, only consumed by auth-svc), `kafka` (KRaft single-node, not consumed by anything yet).

**Auth flow, end to end**: `POST /auth/register` (argon2id-hashed password, 6-digit emailed code) → `POST /auth/verify-email` (activates the account) → `POST /auth/login` (returns a 15-minute HS256 JWT access token + a 7-day single-use Redis-backed refresh token) → `POST /auth/refresh` (rotates the pair) → `POST /auth/logout` (revokes the refresh token). Every route except the pre-auth ones above (plus `/`/`/healthz`) requires `Authorization: Bearer <access token>` at the gateway.

**Not yet built**: gRPC contracts in `proto/` (scaffolding only, no service uses it), the 5 non-auth services' actual business logic, `/auth/forgot-password` + `/auth/reset-password` on auth-svc (routes are reserved at the gateway already), rate limiting, service discovery, automated tests, and lint in CI.
