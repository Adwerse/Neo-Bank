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

## Not yet committed — Dev infrastructure: Postgres, Redis, Kafka
- Added to `docker-compose.yml`: `postgres:16-alpine` (named volume for persistence, dev credentials via env vars, `pg_isready` healthcheck), `redis:7-alpine` (`redis-cli ping` healthcheck), and Kafka running single-node KRaft (no Zookeeper). None of the 7 services connect to this infra yet — that's scoped to future per-service work (ledger-svc, transfers-svc, etc.).
- Documented in `README.md` that the Postgres credentials are for local dev only, not production.
- **Deviation from plan**: the task asked for `bitnami/kafka`, but Bitnami discontinued free Docker Hub distribution for that image (moved it behind a paid "Secure Images" subscription) — confirmed via the Docker Hub API, which now returns zero tags for `bitnami/kafka`. Substituted `bitnamilegacy/kafka:3.7.1`, the frozen, still-free archive of the same image, with the same KRaft environment variables.
- Verified: all 10 containers (7 services + 3 infra) come up together; `postgres` and `redis` report `healthy` in `docker compose ps`; created and listed a test topic via `kafka-topics.sh` inside the Kafka container; Kafka logs confirm a clean KRaft startup.
