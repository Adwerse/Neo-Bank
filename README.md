# Neo-Bank

A mini neobank on a microservices architecture.

Neo-Bank is a neobank pet project: you can register, verify your email with a
six-digit code, top up with a test card, transfer money to another user, and
see the balance update without reloading the page. It's not a product — you
can't open a real account on it, and no transfer ever leaves the test
environment. The point of the project is to genuinely understand how a
banking system works: how a transfer can't push an account negative even
under concurrency, what happens when the database goes down mid-day, and how
to catch a fraudulent transfer before the money leaves.

Technically it's Go microservices behind a single Gateway (JWT, reverse
proxy, WebSocket pushes), a React frontend, and a data layer built on
Postgres (a three-node cluster with automatic failover), Kafka (inter-service
communication via a transactional outbox), and Redis. Every step of the
project answered one concrete engineering question and ended in a
measurement, not an assumption: "can a transfer push an account negative
under concurrency" — no, verified with a concurrency test; "does the system
survive the primary Postgres dying" — yes, ~24 seconds of write downtime,
reproduced several times in a row; "can one transfer be traced across four
services" — yes, one `trace_id` in Jaeger; "what breaks first under load" —
the `ledger-svc` connection pool, with numbers below. The list of what was
deliberately not done, or done as a simulation, is also part of the project,
not an afterthought: see "Honest limitations" below.

## Architecture

```
Browser (React SPA, :5173 in dev mode)
    |
    |  HTTP + WebSocket, JWT in Authorization / in the first WS message
    v
Gateway :8080 -- the single entry point: terminates JWT, proxies by prefix, pushes WS updates
    |
    +--> auth-svc :8081            register / verify-email / login / refresh / logout
    +--> accounts-svc :8082+9082   GET /accounts/me (balance)
    +--> transfers-svc :8084       transfers / deposits / withdrawals / operation history
    +--> fraud-svc :8085+9085      only /healthz (the service itself isn't called through the Gateway)
    +--> notifications-svc :8086   only /healthz (the service has no public API)
    `--> /ws                       WebSocket: signals like "something about you changed"

The path of a single transfer (what every load test measured):

  Gateway --POST /transfers/--> transfers-svc --gRPC--> accounts-svc   (resolving accounts)
                                               --gRPC--> fraud-svc      (CheckTransfer)
                                               --gRPC--> ledger-svc     (ExecuteTransfer)
                                               --REST--> Stripe         (deposits only)

The async side (outbox -> Kafka -> consumers):

  auth-svc, transfers-svc --outbox--> Kafka :9092 --> accounts-svc (account creation)
                                                   --> notifications-svc (emails via Mailpit)
                                                   --> Gateway (WS pushes to the browser)

Infrastructure that almost every service talks to:

  Postgres  -- not a single node but a cluster: pg-node1/2/3 (Patroni) + etcd (consensus) +
               pg-haproxy :5432/5433/5434 (the single address services actually know)
  Jaeger    -- :16686 UI, :4317 OTLP/gRPC -- traces every hop above
  Redis     -- :6379 -- auth-svc only (refresh tokens, rate limit)
  Mailpit   -- :1025 SMTP / :8025 UI -- an email sink instead of a real provider
```

## Quick start

### Requirements
Docker Desktop (with Compose v2); Go 1.25+, only if you'll be running tests
outside containers; Node 18+ for the frontend.

### Bring up the whole backend with one command
```bash
cp .env.example .env       # Stripe placeholders — the stack starts fine without real keys,
                            # but deposits (see DEMO.md) won't actually go through without them
docker compose up -d
```

This brings up 17 containers: 7 application services (`gateway`, `auth-svc`,
`accounts-svc`, `ledger-svc`, `transfers-svc`, `fraud-svc`,
`notifications-svc`) and 10 infrastructure ones (`pg-node1/2/3` + `etcd` +
`pg-haproxy` instead of a single Postgres, `redis`, `kafka` + `kafka-init`,
`mailpit`, `jaeger`). On a clean machine the first build compiles all the Go
binaries and takes a few minutes; a repeat `up` with images already built
takes under a minute. `docker compose ps` should show 16 containers `Up`
(the ones with a healthcheck as `healthy`) and one, `kafka-init`,
`Exited (0)` — that's one-shot Kafka topic provisioning, not a failure.

| port | service | what it is |
|---|---|---|
| 8080 | gateway | **API entry point** — every browser request goes here |
| 5173 | frontend (dev) | **UI entry point** — after `npm run dev` in `frontend/` |
| 16686 | jaeger | **Jaeger UI** — traces |
| 8025 | mailpit | **Mailpit UI** — emails instead of a real mailbox |
| 1025 | mailpit | SMTP sink (services send emails here) |
| 5432 / 5433 / 5434 | pg-haproxy | Postgres: current leader / any standby / synchronous standby |
| 6379 | redis | auth-svc only |
| 9092 | kafka | debugging only (`kafka-console-consumer` etc.) |
| 8081–8086, 9082, 9085 | auth/accounts/ledger/transfers/fraud/notifications-svc | internal — called through the Gateway; exposed on the host only for manual inspection (see sections below) |

### Bring up the frontend
```bash
cd frontend
npm install
npm run dev     # http://localhost:5173
```
The backend comes up separately (see above) — the frontend proxies `/api/*`
to the Gateway through Vite's dev proxy (see "Frontend" below).

### Tests
Unit and integration tests run against a real Postgres (repo convention: a
test skips itself if `DATABASE_URL` isn't set) and assume
`docker compose up -d` is running. The repo is a Go workspace (`go.work`),
but `./...` from the root doesn't expand across all modules (there's no
`go.mod` of its own at the root, only `go.work`) — the same way CI
(`.github/workflows/ci.yml`) does it, via `go list -m`:
```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go list -m -f '{{.Dir}}' | while IFS= read -r dir; do (cd "$dir" && go test ./...); done
```
(`go test all` technically works too, but expands the entire dependency
graph and doesn't finish in a reasonable time on this repo — don't use it.)
Tests for a single service — as everywhere else in this README below, `cd
services/<name> && go test ./... -v` or `DATABASE_URL=... go test
./services/<name>/... -v` from the root.

Separately, **not** part of the regular run: the failover test, which really
does kill the Postgres container —
```bash
FAILOVER_TEST=1 go test ./infra/failover/... -v -count=1 -timeout 15m
```
Details — "Postgres: replication and automatic failover" below.

### Load test
```bash
go run ./loadtest/cmd/lt setup -users 40 -fund 100000000
go run ./loadtest/cmd/lt fraud -mode loadtest
./loadtest/run.sh all
go run ./loadtest/cmd/lt fraud -mode restore
```
Three profiles (distributed / hot account / duplicates); results and a
breakdown of the bottlenecks — "Load testing" below (ceiling ~176 tx/s, hot
account 31.5 tx/s regardless of concurrency, the bottleneck is the
`ledger-svc` connection pool).

### Jaeger and Mailpit
- **Jaeger** — http://localhost:16686. Make a transfer (via the UI or curl),
  pick `service=gateway`, find the trace — inside it are the nested spans of
  every service it passed through.
- **Mailpit** — http://localhost:8025. Every email (the email confirmation
  code, transfer notifications) lands here instead of a real SMTP server —
  nothing leaves the docker network.

A step-by-step demo script (5–10 minutes, from registration to a Postgres
failover) lives separately in [DEMO.md](DEMO.md). Screenshots/gifs of the key
screens and the Jaeger trace aren't inserted into this README yet — a
checklist of what to capture and where to put it is in
[docs/screenshots/CHECKLIST.md](docs/screenshots/CHECKLIST.md).

## Honest limitations

compromise, not forgotten work. The framing is always the same: "I know and
chose this," never "I didn't get to it."

- **Withdrawals are simulated.** `POST /withdrawals` really does debit the
  internal balance (through the same `ledger-svc.ExecuteTransfer` as an
  ordinary transfer), but no call to the Stripe payout API ever goes out —
  the row gets a `payout_simulated` status, visible both in the API and in
  the operation history. A real payout to a card/account (Stripe Connect,
  ACH) requires a money transmitter license — that's a regulatory limit, not
  a technical one, and it can't be worked around even running entirely in
  Stripe test mode. Details — "`POST /withdrawals` — withdrawing money,
  SIMULATION ONLY" below.

- **The bank code in the IBAN belongs to a nonexistent institution.**
  `accounts-svc` generates a real Irish IBAN for every account that passes
  the check-digit validation (ISO 7064 mod-97-10, implemented in
  `pkg/iban`) — but with the 4-letter bank code `ZZZZ` (`BANK_CODE` in
  config), not the code of a real Irish institution (`AIBK`, `BOFI`, `PTSB`
  and the like). This is a deliberate choice, not an oversight: generated
  bank details must not point at someone else's real organization. The IBAN
  is deterministic from the already-issued `account_number` (see
  `services/accounts-svc/accounts.go`, `ibanPartsFromAccountNumber`) and is
  backfilled for accounts created before that — see
  `services/accounts-svc/backfill.go`.

- **Transfers are intra-bank only — no SEPA, and never will be.** `POST
  /transfers` resolves the recipient by IBAN via
  `accounts-svc.ResolveAccountByIban`, but only accepts an IBAN carrying the
  bank's own code (`BANK_CODE`) — a structurally valid IBAN from another
  institution is rejected with its own distinct message ("transfers are only
  supported within this bank"), not silently treated as "recipient not
  found." A real interbank transfer (SEPA Credit Transfer) requires
  membership in a clearing system — not a technical limitation, an
  architectural decision not to build what there's no need to demonstrate
  here. Details — "Resolving a recipient by IBAN" below.

- **The recipient's name isn't shown when confirming a transfer.** Not
  because it didn't occur to anyone: the system has no name field for a user
  at all — registration (`auth-svc`) only collects an email and a password,
  and that same field would have been the exact same oracle as the resolve
  call itself. Real banks show the name to protect against a typo in the
  details — that feature isn't implemented here. The reasoning behind the
  choice among three options (don't show it / show it partially / show it
  behind a hard rate limit) — "Resolving a recipient by IBAN" below.

- **There's no KYC.** Registration verifies ownership of a mailbox (a
  six-digit code by email) — and that's it. Identity checks (a document,
  a selfie match, sanctions lists, PEP screening) are absent from the
  project and were never planned: that's a separate regulatory and
  integration domain (typical vendors are Persona, Sumsub, Onfido) that
  doesn't overlap with the questions this project answers — concurrency,
  fault tolerance, traceability, load. Email verification proves mailbox
  ownership, not identity; the project never passes one off as the other.

- **The credentials in `docker-compose.yml` are for local development
  only.** The Postgres password and `JWT_SECRET` are readable hardcoded
  values in a file that's committed. That's deliberate: secrets that must
  be real (Stripe keys) are pulled out into `.env` and into `.gitignore`;
  secrets only needed to bring the stack up locally, identical for everyone
  who clones the repo, aren't worth hiding — they've been "public" from the
  moment they became a file in the repository. In production: a secret
  manager, not `docker-compose.yml`.

- **The refresh token lives in `localStorage`, not an httpOnly cookie.**
  `localStorage` is readable by any JS on the page, including anything
  injected via XSS. The correct fix is an httpOnly cookie (JS physically
  can't read it), but that requires changing the `TokenPair` contract in
  `gateway/openapi.yaml`, turning the `/login`/`/refresh` responses into
  `Set-Cookie`, adding a `SameSite`/`Secure` policy, and teaching the
  Gateway to read a cookie, not just the `Authorization` header. The current
  setup is a deliberately accepted short-term compromise with a real cost,
  not how it should stay. The access token, meanwhile, lives only in memory
  (not `localStorage`) — half a mitigation, not a full fix. Details —
  "Token storage — and its cost" below.

- **Exactly-once for emails is unreachable in principle — at-least-once
  with deduplication was chosen instead.** There's no atomic operation
  between "the email was sent" and "Kafka delivery was confirmed": either a
  duplicate email is possible (sent it, then crashed before committing the
  offset — a retry sends it again), or a loss is possible (committed, then
  crashed before sending — the email never goes out at all). The first risk
  was chosen: an idempotency barrier (`processing` → send → `sent`)
  minimizes but doesn't eliminate the duplicate window, while it eliminates
  the loss window entirely. For a bank that's the right side to err on — a
  missed transfer notification is worse than a duplicate one.

- **Fail-closed when fraud-svc is unavailable, not fail-open.** If
  fraud-svc doesn't respond (crashed, timed out, a Postgres error on its
  side), the transfer doesn't go through — it stays `pending`, the money
  doesn't move, the client gets a `202` with an explanation. The
  alternative (fail-open — let the transfer through without a check) is
  more available, but opens a hole: anyone who knows fraud-svc can be taken
  down (or simply hits the window of a real outage) could push a transfer
  through with no check at all. The cost of this choice is real: a transfer
  doesn't complete until fraud-svc responds, and hangs in `pending` until
  the reconciliation worker or a recovered fraud-svc resolves it.

- **The hot account is bottlenecked by a row lock — a measured number, not
  a hypothesis.** `SELECT ... FOR UPDATE` on the recipient account
  serializes every transfer that touches it: 31.5 transfers/s, and that
  number doesn't change from 10 to 120 concurrent users — latency grows
  linearly instead. This exact lock is what keeps the account from going
  negative under concurrency (the sprint 3 test); the alternatives (balance
  sharding, optimistic locking with retries, netting) buy throughput at the
  cost of either that guarantee or schema simplicity — and weren't done here
  on purpose, not for lack of time. Full breakdown with traces — "Load
  testing" below.

- **There's no mTLS or shared secret between services.** Internal
  gRPC/HTTP calls (`transfers-svc` → `fraud-svc`, Gateway → `accounts-svc`,
  etc.) are completely unauthenticated today — anyone who ends up inside
  the docker network can call `ledger-svc.ExecuteTransfer` directly,
  bypassing the Gateway and the fraud check. Inside Docker with a closed
  network this isn't a working attack vector, but it's the kind of
  compromise that wouldn't survive a move to an environment with broader
  network access.

- **100% trace sampling is for local development only.** Not an assumption:
  `jaeger all-in-one` with in-memory storage used 13.3 of 15.5 GiB of host
  memory after ~60 thousand traced transfers during the load test, and
  triggered side-effect DNS timeouts and a brief Patroni outage. In
  production: persistent storage (Cassandra/Elasticsearch) with ratio
  sampling, or a head/tail sampler prioritizing errors/slow traces, not
  100%.

## Architecture decisions (mini-ADRs)

Ten decisions worth making deliberately rather than by default — what was
chosen, what was rejected, and why.

### A monorepo, not per-service repositories
**Chosen:** one repository, one Go workspace (`go.work`) for seven
services, `pkg/{health,outbox,pgha,tracing}` as shared modules, `proto/gen/go`
as shared contracts.
**Rejected:** a separate repository per service — the usual choice for an
organization with separate teams and independent deploys.
**Why:** this project isn't a set of independently evolving teams, it's one
system that one person changes across several services in a single step at a
time (example — tracing: one package, seven `go.mod` files, a change in
`pkg/pgha.NewPool` is picked up by every service at once via `replace`).
Polyrepo buys release-cycle independence — there's no release cycle here in
the usual sense at all, and that independence would have turned into manual
version syncing of shared packages across seven repositories.

### A custom Gateway, not Traefik/Kong
**Chosen:** `gateway/` — an ordinary `net/http` service: `httputil.ReverseProxy`
by path prefix plus its own JWT middleware plus a WebSocket endpoint with the
same authentication.
**Rejected:** an off-the-shelf API gateway (Traefik, Kong, Envoy) with a
JWT plugin.
**Why:** JWT validation, WS authentication on the first message, and routing
here aren't three independent concerns sharing infrastructure, they're one:
`parseAccessToken` is used directly by both the middleware and the WS
handler as an ordinary Go function, with no decision serialized through an
external plugin API. An off-the-shelf gateway would have handled routing
just as well, but at the cost of a second configuration language
(Traefik labels/Kong plugins) for logic that reads more clearly in a few
hundred lines of Go and tests like ordinary code
(`gateway/ws_test.go`, `gateway/notify_test.go`), rather than through
integration tests against someone else's runtime. At the scale of dozens of
services and several teams, this decision would be worth revisiting.

### Kafka, not NATS
**Chosen:** Kafka (via `segmentio/kafka-go`) as the single event bus.
**Rejected:** NATS / NATS JetStream — simpler to operate, lower latency.
**Why:** two requirements exist in this system from the very first event,
and NATS Core has neither at all, while JetStream adds them as a separate
layer on top: key-based partitioning with ordering guaranteed within a
partition (`user_id`/`sender_account_id` — events for one account must be
processed in order) and replay from the beginning for compacted topics
(`user.events`/`account.events` — a new consumer must see the full history
per key, not just the last N messages). That's core to Kafka's model, not
an add-on. The cost — Kafka is noticeably heavier to operate (visible in the
load test too: the outbox relay bottlenecks on `kafka-go`'s default
`BatchTimeout=1s`, see "Bottleneck #3" below) — and that's a deliberately
accepted cost, not an unnoticed side effect.

### An event, not a synchronous call, when creating an account
**Chosen:** `auth-svc` publishes `UserActivated` to `user.events` after
email verification; `accounts-svc` is an async consumer that creates the
account off that event and then calls `ledger-svc` over gRPC.
**Rejected:** `auth-svc` calls `accounts-svc` synchronously (REST/gRPC)
directly from the `/verify-email` handler.
**Why:** a synchronous call would have made email verification depend on
`accounts-svc`'s uptime (and transitively `ledger-svc`'s) at the exact moment
a user is waiting on a response in the browser. With an event, `/verify-email`
commits and responds `200` without waiting for the account to be ready
somewhere in the system; account creation happens-after, not blocking-after.
The cost — a user could theoretically land on the dashboard in the window
between the email being confirmed and the event being processed, while the
account doesn't exist yet; the window is bounded by the outbox relay's
interval (a once-a-second ticker) — the same at-least-once idempotency that
protects the whole Kafka layer makes this delivery reliable too, with no
extra code.

### An outbox, not a direct publish to Kafka
**Chosen:** the event is written to an outbox table in the same Postgres
transaction as the business change; a separate relay worker publishes it to
Kafka a second or so later.
**Rejected:** publishing to Kafka directly from the handler, right after the
business transaction's `COMMIT` — that's actually how auth-svc was
historically wired, before the migration to an outbox.
**Why:** a direct publish leaves a window nothing can close: if the process
crashes between the `COMMIT` and the call to the Kafka producer, the event is
lost silently — the business change happened, and the system never found
out. An outbox makes the publish part of the same atomic unit as the change
itself (either both happen, or neither does), at the cost of the publish
becoming delayed by a second or two instead of instant — a cost measured in
the load test ("Bottleneck #3"): with default settings, the relay falls
176x behind the load.

### A signal, not data, in WebSocket messages
**Chosen:** `{"type":"balance.changed"}`,
`{"type":"transfer.updated","transfer_id":"..."}` — only the event type and
an identifier; the client re-fetches the authoritative value over ordinary
HTTP.
**Rejected:** putting the actual new balance/status in the WS message
itself, saving the client an extra HTTP request.
**Why:** WS delivery order isn't guaranteed (an event can arrive after a
later one, or not arrive at all — e.g. if the connection dropped between
sending and delivery), which means any value placed directly in the message
could silently be stale — the client can't tell fresh data apart from stale
data that looks fresh. A signal has none of this problem by construction:
the HTTP request it triggers always goes through `jwtMiddleware` and always
reads current state. The cost — every change costs two round trips (a WS
push + an HTTP request) instead of one, and that's deliberately accepted for
the sake of consistency, not missed by oversight.

### `succeeded` and `credited` — two distinct deposit statuses, not one
**Chosen:** a deposit goes through `succeeded` (Stripe accepted the
payment) → `credited` (a background worker posted the ledger entry) as two
separate, observable statuses with a gap between them.
**Rejected:** a single `completed` status, set directly in the Stripe
webhook.
**Why:** these are two distinct facts, physically happening at different
times in different systems — Stripe confirmed the card charge, Neo-Bank
credited the balance — and collapsing them into one status would be untrue
during the seconds when the webhook has already arrived but the background
worker hasn't posted the transaction yet. The frontend uses this honesty
directly: the deposit screen doesn't show "balance topped up" on the first
success, it polls `GET /deposits/{id}` until the status becomes `credited`.
The trade-off is more complexity (a background worker is needed,
reconciliation for stuck `succeeded` deposits), but the alternative would
mislead the user at exactly the moment trust in the balance figure matters
most.

### No MongoDB — or any other NoSQL database
**Chosen:** one Postgres (cluster) for everything, including what looks
like unstructured data (JSONB columns — `fraud_checks.details`,
`outbox.trace_context`).
**Rejected:** a document database for specific services — say,
notifications-svc with its `user_contacts` projection, or fraud-svc with
`details`, which is already JSONB.
**Why:** the heart of the system is double-entry (`entries`), and its
invariant (`SUM(entries) = 0`, no balance ever going negative under
concurrency) can't be expressed without strict ACID transactions and
row-level locks — something a document database doesn't provide by
definition of its consistency model. Any service that stood up a separate
NoSQL database would gain nothing fundamental: it either doesn't
participate in the money invariant (notifications-svc, fraud-svc), and
Postgres with JSONB for its data is no worse than a document database, or it
does participate — and then ACID is mandatory, question closed. The cost of
"one Postgres for everything" is independent scaling of each service by its
own load profile; at this project's scale, nobody pays that cost.

### Profile lives in auth-svc, not a separate profile-svc
**Chosen:** `display_name`/`avatar_key`/`avatar_updated_at` are columns on
`users` in auth-svc, served through `GET /profile`/`PATCH /profile` there
too.
**Rejected:** a separate `profile-svc` with its own DB, its own outbox, its
own migrations.
**Why:** auth-svc already owns the user entity — the profile (display name,
avatar) is an attribute of it, not an attribute of the account
(`accounts-svc`) and not a standalone entity with its own lifecycle.
Service boundaries in this project follow data ownership (see "Monorepo"
above, the same principle), not every new pair of fields: a `profile-svc`
would be cleaner in terms of responsibility boundaries on paper, but for two
columns it would mean standing up a second copy of all the plumbing
auth-svc already has and already runs — a Postgres connection, migrations,
`auth_outbox` + relay + cleanup worker, health check — and giving the
consuming service (`notifications-svc`) another gRPC/HTTP client and point
of failure for data that logically still belongs to the user auth-svc
already owns. That's why `ProfileUpdated` is published through the existing
`auth_outbox`/`user.events`, the same path as `UserActivated` — not a new
topic, not a new table.

### Profile screen: parallel requests from the frontend, not an aggregating Gateway endpoint
**Chosen:** the profile screen hits three data sources (`GET /profile` —
auth-svc; `GET /accounts/me` — accounts-svc, which already aggregates the
account, IBAN, and balance via `ledger-svc`) as two independent HTTP
requests; the frontend (whenever it gets there — see this sprint's "NOT TO
DO") merges them via react-query.
**Rejected:** a BFF endpoint in the Gateway (`GET /profile-screen` or
similar) that makes both calls itself and returns one assembled JSON.
**Why:** a BFF has exactly one real argument in its favor — fewer round
trips over a mobile network, which has a real cost on a bad connection, but
not in this system at this stage. In exchange, the Gateway stops being a
thin proxy (see "A custom Gateway" above — its job today is routing, JWT,
WebSocket, nothing domain-specific) and gains knowledge of what a "profile
screen" even is — a domain concept the transport layer shouldn't really
know about. Two independent requests instead of one aggregating call is
also what directly gives task 3 below for free: if `auth-svc` is
unavailable, `GET /accounts/me` — a separate TCP connection to a separate
backend — simply won't notice; an aggregating endpoint, by contrast, would
have had to decide FOR ITSELF what to do about its own partial failure
(wait on both calls with a shared timeout? return `207 Multi-Status`?
silently swallow one domain's error?) — not free logic, and exactly what the
parallel-request approach avoids by construction. **Had a BFF been chosen**
— both calls (auth-svc, accounts-svc) would have to go out from the Gateway
in parallel (a `goroutine` + `errgroup`/`sync.WaitGroup` for both calls,
waiting on both, not one after another), or the real screen's latency
becomes the sum of the two services rather than the max of them; but that's
a hypothetical design, not what's actually built here.

### Manual verification: partial degradation of the profile screen
```bash
# Everything up — both requests respond:
curl -s http://localhost:8080/profile -H "Authorization: Bearer $ALICE_TOKEN"
curl -s http://localhost:8080/accounts/me -H "Authorization: Bearer $ALICE_TOKEN"

# Kill auth-svc — the balance must not be affected:
docker compose stop auth-svc
curl -s -o /dev/null -w "%{http_code}\n" http://localhost:8080/profile \
  -H "Authorization: Bearer $ALICE_TOKEN"
# 502/503 — the Gateway can't reach auth-svc
curl -s http://localhost:8080/accounts/me -H "Authorization: Bearer $ALICE_TOKEN"
# 200, IBAN and balance are intact — accounts-svc and ledger-svc know nothing
# about auth-svc and never call it for anything on this path
docker compose start auth-svc
```
This works with not one line of special-case code: `jwtMiddleware` verifies
the JWT locally (HMAC over `JWT_SECRET`, the same secret auth-svc signed it
with at login) — a token issued before auth-svc went down keeps passing
verification afterward too, since validation never goes back to auth-svc for
confirmation. `GET /accounts/me`, for its part, makes zero calls into
auth-svc — the account, IBAN, and balance come entirely from accounts-svc
and ledger-svc. A failure in one domain can't leak into the other, because
there's no call between them on this path at all.

## Layout
- `gateway/` — the single entry point (API Gateway)
- `services/` — microservices: `auth-svc`, `accounts-svc`, `ledger-svc`, `transfers-svc`, `fraud-svc`, `notifications-svc`
- `proto/` — shared protobuf contracts between services
- `infra/patroni/` — the Patroni image and config (manages all three Postgres nodes), `infra/haproxy/` — routing to the current leader, `infra/postgres/` — the lag-monitoring query, `infra/failover/` — the failover test; all of this — see "Postgres: replication and automatic failover" below
- `frontend/` — the SPA (Vite + React + TypeScript), see "Frontend" below
- `.github/workflows/` — CI pipelines

## Infrastructure (dev)
Postgres, Redis, and Kafka are brought up in `docker-compose.yml`. Postgres is used by every service that has its own schema (everyone but the gateway). Redis is auth-svc only (sessions/tokens). Kafka has auth-svc, accounts-svc, and transfers-svc as producers, accounts-svc and notifications-svc as consumers (see "Events (Kafka)" below); notifications-svc additionally publishes to `transfer.events.dlq` (see "notifications-svc: consumer resilience"), so technically it's now a producer too, but only for its own dead-letter topic, not for domain events.

Postgres isn't a single container but a three-node cluster under Patroni with automatic failover, plus etcd as the DCS and HAProxy as the entry point; services connect to `pg-haproxy:5432` and know nothing about the individual nodes. Details — in "Postgres: replication and automatic failover" below.

Jaeger collects traces from every service (UI — http://localhost:16686), see "Tracing" below. Nothing deliberately depends on it: an unavailable Jaeger means no traces and nothing more.

The Postgres credentials in `docker-compose.yml` are for local development only, not production. The same goes for the single-node etcd: production needs a quorum of 3+ nodes, or the DCS itself becomes a single point of failure.

MinIO is an S3-compatible object store (the `avatars` bucket, created by a one-shot `minio-init` on startup, the same trick `kafka-init` uses to create topics). `auth-svc` is the only service with access to it: avatar upload via a presigned URL, details — "Avatar upload" below. Credentials are dev-only, like Postgres's. The S3-compatible API isn't an accident — it's the reason MinIO was chosen for dev: moving to production means Cloudflare R2 or AWS S3 on the same protocol, i.e. swapping the endpoint/credentials in `auth-svc`'s config, not rewriting code.

## Postgres: replication and automatic failover (Patroni + etcd)
A single Postgres instance is a single point of failure for the whole system. Streaming replication (previous sprint) removed the risk of data loss, but not the downtime: if the primary went down, the system stayed down until someone manually promoted a replica. That's not HA yet.

Now the cluster is **three equal nodes** (`pg-node1`, `pg-node2`, `pg-node3`) under **Patroni**, which watches the nodes and, when the leader goes down, promotes a standby itself. An important difference from the previous sprint: nodes are no longer called "primary" and "replica." Who's the leader, who's the synchronous standby, and who's the asynchronous one is Patroni's decision, stored in etcd, and **it changes at runtime**. Roles baked into container names would be a lie after the very first failover.

- **`etcd`** — the DCS (Distributed Configuration Store), a consensus-backed store.
- **`pg-node1/2/3`** — Postgres 16 + Patroni in one container (`infra/patroni/Dockerfile`).
- **`pg-haproxy`** — the single entry point for the application (`infra/haproxy/haproxy.cfg`).

### Why a DCS: without consensus, autofailover is a split-brain generator
A node that loses contact with the leader can't tell "the leader died" apart from "I got cut off from the network." If it promotes itself on that guess, you get **two nodes accepting writes**, and two diverging histories. For a ledger that's a catastrophe: both histories look valid, `SUM(entries)` balances in each one, and the total amount of money has increased — recovery is only possible manually, by diffing the two WALs.

etcd removes the guesswork: to become leader, a node has to take a key that physically only one node can hold, and a minority that ends up isolated can't reach quorum and won't take it.

**A single-node etcd here is a deliberate simplification for local development.** A single node has no quorum to lose — meaning it's itself a single point of failure, exactly the one this sprint removed, just one layer down. An etcd outage doesn't cause split-brain (Patroni demotes the leader when it loses the DCS — that's the safe direction), but writes stop until etcd comes back. **Production needs 3 or 5 etcd nodes across separate failure domains.**

### Patroni config: what actually matters here
The whole config is `infra/patroni/patroni.yml`, one file for all three nodes (the differences are three environment variables in `docker-compose.yml`, the same way `replica-entrypoint.sh` used to work). Two blocks deserve special attention.

**`synchronous_mode: true` + `synchronous_node_count: 1`** is what makes a failover safe for the ledger, and what the previous sprint paid synchronous-replica latency for.

Without `synchronous_mode`, Patroni promotes the candidate with the highest LSN. That node might be missing transactions the primary **already confirmed to the client**: the client was told "committed," the money moved, and the new leader never heard about it. That's not "a bit of lag," that's a torn transaction.

With `synchronous_mode`, Patroni tracks the set of synchronous standbys both in `synchronous_standby_names` and in the `/sync` key in etcd, and **refuses to promote a node that isn't in that key**. And since the leader can't confirm a commit until a synchronous standby has flushed it to disk, any confirmed transaction physically lives on the one node that's even eligible to become leader. The entire chain of reasoning about "no confirmed transaction is ever lost" rests on this, and it's exactly what `infra/failover/failover_test.go` verifies.

`synchronous_node_count: 1` reproduces the previous sprint's topology exactly: of two standbys, one synchronous, one asynchronous.

**`synchronous_mode_strict` is NOT enabled.** Non-strict means: if the *last* synchronous standby disappears, the leader degrades to asynchronous commits and keeps accepting writes. Strict would block writes, preserving the "committed on at least two nodes" guarantee absolutely, at the cost of availability. With three nodes, killing one doesn't trigger non-strict behavior at all (the second standby remains). A real bank probably needs strict — but that should be a deliberate decision, not an inherited default.

**The failover budget** — `ttl: 20`, `loop_wait: 3`, `retry_timeout: 5`. Patroni requires `ttl >= loop_wait + 2*retry_timeout`, or a merely slow leader (a GC pause, a stalled disk) wouldn't refresh its key in time and would get demoted while healthy. Detection costs up to `ttl` seconds — nothing can be promoted until the dead leader's key expires, and that's the main lever on failover time. **20 isn't a free choice, it's Patroni's hard minimum**: anything smaller is silently raised to 20 with a single log line (`WARNING: ttl=15 can't be smaller than 20, adjusting...`), so a config promising a 15-second budget would be a fiction.

### Entry point: HAProxy, not target_session_attrs
After a failover the leader's address changes, so a service can't hardcode a host. Two options were considered; HAProxy was chosen.

The alternative — `target_session_attrs=read-write` with a list of every host in the connection string: the driver walks them itself and finds the one accepting writes. pgx supports this, and it's a working option. Two arguments against it, and the second one is decisive:

1. **The connection string stops being a single string.** A list of three hosts would have to be kept in sync in six places in `docker-compose.yml`, in every `DATABASE_URL` in the README, in `cmd/seed`, and in `cmd/devtopup`. With HAProxy, the address stays a single one (`pg-haproxy:5432`), and the host ports are the same they were before Patroni — not one command in the README changed.
2. **It's the wrong question, asked of the wrong party.** `target_session_attrs=read-write` asks Postgres `SHOW transaction_read_only` — i.e. "are you currently accepting writes." HAProxy asks Patroni `GET /primary` — "does the consensus layer consider you the leader right now." Those aren't the same thing: a node can answer "I'm writable" at the exact moment Patroni is already demoting or fencing it. For a ledger, the right question goes to the layer that owns the leader key, not to a process that hasn't found out about that key yet.

`pg-haproxy` ports:

| port | who | what for |
|---|---|---|
| 5432 | current leader | `DATABASE_URL` — writes and anything read-your-writes-sensitive |
| 5433 | any standby | `DATABASE_URL_REPLICA` — see the read-splitting rule below |
| 5434 | current synchronous standby | inspection and tests only |
| 7000 | stats | also the source of the container's own healthcheck |

Two settings in `infra/haproxy/haproxy.cfg`, without which the whole construction doesn't work:

- **`on-marked-down shutdown-sessions`** — this is what buys "pools reconnect instead of staying dead," and it's bought here, not in Go code. Without it, TCP connections pgxpool is already holding to the old leader stay ESTABLISHED after the node fails its health check: the pool keeps handing out connections to a node that's gone, and each one only fails when it's actually used. With it, HAProxy tears down those sessions the moment a node is marked down, the client gets an immediate connection error, and pgx destroys the failed connection instead of returning it to the pool — and the pool refills from the new leader.
- **`resolvers docker`** — by default HAProxy resolves `server` hostnames **once at startup** and caches the address forever. Docker gives a recreated container a new IP. The result: the proxy reports "healthy," while every backend shows L4CON, because it's talking to addresses where nobody's listening. This isn't hypothetical — it actually happened after rebuilding the Patroni image, and it would have broken the killed node coming back at the end of the test too.

### Application-side handling
Shared code is `pkg/pgha`. A failover is deliberately **not hidden**: it's a few seconds where connections drop and requests fail. The goal is for everything to recover on its own by the end of that window.

- **`pgha.NewPool`** — a pool tuned for a changing leader: `HealthCheckPeriod` 10s instead of the default one minute (pgx's background goroutine discards broken idle connections faster), `ConnectTimeout` 5s (otherwise dialing a vanished node runs into the OS's TCP timeout — minutes), moderate `MaxConnLifetime`/jitter.
- **`pgha.WaitForWritable` + `pgha.Retry`** — the order in each service's `main()` is flipped: pool first, then migrations. The pool is what lets you ask "is there a leader yet?", and `WaitForWritable` blocks until the answer is yes. A service that started mid-failover now waits it out instead of dying trying to migrate against a standby. `pgha.Retry`, meanwhile, **returns a real migration error immediately** — broken SQL shouldn't turn into two minutes of silence.
- **`pgha.IsUnavailable`** — the classifier for "the database is unreachable" versus "the query is wrong." The least obvious spot in the package, and the source of a real bug (see below).

Long-lived consumers were verified separately, because for them "it works" means "survived without a manual restart":

- **The outbox relay and reconciliation workers** already survived a failover before — they log the error and retry on the next tick. During the window that's ~1 log line a second per service; noisy, but the right kind of noise: it shows exactly what's happening.
- **`accounts-svc`'s `user.events` consumer** — on a `FetchMessage` error it spun with no delay (`continue` with no `time.Sleep`), unlike its counterparts in `notifications-svc`. During a failover that's a hot loop drowning the one useful log line. Added `fetchErrorBackoff` and a `ctx.Err()` check.
- **`notifications-svc`'s `transfer.events` consumer** — this one had a **real data-processing bug**. The retry ladder (5 attempts, 0.5/1/2/4s ≈ 7.5s) was sized for "SMTP blinked," after which the message goes to the DLQ as poison. A failover runs longer than that window, meaning **the very first failover would have sent a batch of perfectly normal transfer notifications to the dead-letter topic**. Nothing would have been lost — that's what the DLQ is for — but "every transfer during a failover needs a manual replay" reads as a poison-message incident instead of a routine event. Now errors for which `pgha.IsUnavailable` is true retry on a separate budget (`transferUnavailableBudget`, 5 minutes) and **don't spend** attempts from the poison ladder.

### The failover test — the centerpiece of this sprint
`infra/failover/failover_test.go`. Plenty of setups bring up replication and never actually test a failover; as a result it doesn't work exactly when it's needed.

The test is gated behind an **explicit opt-in**, not this repo's usual `DATABASE_URL`-presence check: it kills a container, and `go test ./...` against a running stack must not take the database down as a side effect.

```
FAILOVER_TEST=1 go test ./infra/failover/... -v -count=1 -timeout 15m
```

What it does: creates ledger accounts through real gRPC, spins up 4 goroutines pouring transfers through `ledger-svc` (i.e. through **the service's own pool**, not a test one), kills the leader's container with `docker kill` (SIGKILL, not `stop` — with a graceful shutdown Patroni hands off the leader key itself, and that's the easy case), then verifies:

1. **No confirmed transaction is ever lost.** Confirmed only: a call that failed or timed out might have committed or might not have, and the application was never told either way — its absence isn't data loss. It checks that every transaction has **two** entries on the new leader: a one-sided entry is formally "present," but it's exactly the torn pair that breaks double-entry.
2. **`SUM(entries) = 0`** — globally across the test's accounts and **separately per `transaction_id`**. The second check is strictly stronger: two equal-and-opposite errors in different transactions would cancel out in the global sum.
3. **The application recovered on its own.** `StartedAt` and `RestartCount` for the `ledger-svc`/`transfers-svc`/`notifications-svc` containers are compared before and after: if Docker had restarted them, "recovered" would prove nothing about the pools. Plus `GET /healthz` on `transfers-svc` (which does a `SELECT 1` on its own pool) is 200 again.
4. **The old leader comes back as a replica, not a second leader.** `leaderName` fails the test if there's more than one leader — meaning this check is itself the split-brain check.

A separate test, `TestSyncStandbyHoldsAcknowledgedTransactionImmediately`, verifies the premise everything else rests on **with no retry and no sleep at all**: the instant `ledger-svc` says "the transfer went through," both entries are already readable on the synchronous standby (port 5434). If this test ever needs a retry, replication isn't synchronous, and every guarantee above collapses. It's kept separate so a break like that says so directly, instead of surfacing later as a mysterious "lost transaction."

### Measured failover time
Three runs on one machine (Docker Desktop / Windows, all three nodes on one host):

| run | killed | new leader | write downtime | |
|---|---|---|---|---|
| 1 | pg-node2 | pg-node1 | **25.4 s** | HAProxy still at `rise 2` |
| 2 | pg-node1 | pg-node3 | **23.6 s** | after `rise 1` |
| 3 | pg-node2 | pg-node1 | **23.6 s** | |
| 4 | pg-node1 | pg-node3 | **24.6 s** | cluster brought up from scratch (`down` + empty volumes) |

Downtime is measured as "first failed write → first write that succeeds again." The first version of the test measured from the moment of `docker kill` to the first success and showed **11 ms** — nonsense: calls sent *before* the kill were still in flight, and some of them committed on the dying leader and returned success a few milliseconds later. The successes were real, but they had nothing to do with recovery.

What the ~24 seconds breaks down into:

- **~20 s — the leader key's TTL expiring.** The dominant part, and it's Patroni's hard minimum (see above). Can't be reduced without rewriting Patroni.
- **~2 s — the election and promotion.**
- **~1–2 s — HAProxy noticing the new leader.** Originally ~3–4 s: the leader's backend was set to `rise 2`, i.e. it required two consecutive successful checks. Switched to `rise 1` — the check here isn't sampling health, it's a confirmation: Patroni answers 200 on `/primary` if and only if it holds the leader key, and only one node ever holds it. A second confirmation adds no information but costs a whole `inter` interval of downtime. `fall` stayed at 2, though: the asymmetry runs the other way there — one missed check really can be a blink, and demoting the leader over it is expensive.

So in production with the default `ttl: 30` this window would be ~35 s, and there's no way to push it below ~22 s on Patroni at all.

### What broke along the way (and why it's worth knowing)
Three things, each of which would have gone unnoticed without the test:

1. **`pgha.IsUnavailable` treated a connect timeout as the caller's own deadline.** pgx tracks `ConnectTimeout` itself, so dialing a dead node returns a `*pgconn.ConnectError` whose chain ends in `context.DeadlineExceeded`. The context-error check was placed before the `ConnectError` check — and classified this as "the caller's deadline, not a database problem," i.e. non-retryable. The result: `WaitForWritable` gave up on the very first attempt, and **all six services crash-looped** on startup against a cluster that was still electing a leader. The regression test is `TestIsUnavailable_ConnectTimeoutIsNotACallerDeadline`, and it reproduces the error for real (a listener that accepts the TCP connection and stays silent), because the cause inside `pgconn.ConnectError` is unexported and the right shape can't be faked.
2. **HAProxy cached node IPs forever** — see `resolvers docker` above.
3. **The test's own checks failed because of the very thing they were checking.** The test's pool held connections to the killed node, HAProxy tore them down, and the first verification query failed with `unexpected EOF` — the test reported data loss where nothing had actually been lost. Verification is now wrapped in `pgha.Retry`: a test about transient connection failures shouldn't fail on a transient connection failure itself.

### Manual verification
```
docker compose up -d

# who's who right now (roles change — that's normal)
docker compose exec -T pg-node1 curl -s http://127.0.0.1:8008/cluster

# exactly one 'sync', the rest 'async'; application_name is Patroni's node name
psql "postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  -c "SELECT application_name, state, sync_state FROM pg_stat_replication ORDER BY 1;"
#  application_name |   state   | sync_state
# ------------------+-----------+------------
#  pg-node2         | streaming | async
#  pg-node3         | streaming | sync

# lag in bytes and seconds
psql "postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  -f infra/postgres/check_replication_lag.sql

# routing: 5432 writes, 5433 is a standby (in recovery)
psql "postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" -tAc "SELECT pg_is_in_recovery();"   # f
psql "postgres://neobank:neobank_dev_password@localhost:5433/neobank?sslmode=disable" -tAc "SELECT pg_is_in_recovery();"   # t

# HAProxy backend status (exactly one UP in primary)
curl -s 'http://localhost:7000/;csv' | awk -F, '$2 ~ /^pg-node/ {print $1,$2,$18}'

# manual failover, no killing involved: a planned switchover
docker compose exec -T pg-node1 patronictl -c /etc/patroni/patroni.yml switchover --force
```

Note: `patronictl switchover` is a **planned** switchover, and it's fast (the leader hands off the key itself, no need to wait out the TTL). It doesn't replace the test: the case that's measured and dangerous is a sudden death.

### Read splitting: why NOT "every SELECT hits a replica"
A tempting but wrong default for a financial app. The failure scenario: a user makes a transfer (writes to the primary) → immediately opens the balance screen → the balance is read from a lagging asynchronous replica → sees the old value → decides the transfer didn't go through, and retries it. That's a textbook read-your-writes violation, and it's not solved by "a cheaper replica," but by an explicit per-query rule:

- **Leader only** — anything read right after a write from the same (or a related) action: the balance (`GET /accounts/me`), a transfer/deposit status right after it's created, the first page of the operations feed (`GET /transfers` with no cursor — exactly what opens right after a transfer/top-up).
- **A standby is fine** — data whose staleness by a few seconds doesn't change the user's decision: operation-history pages beyond the first. `GET /transfers?cursor=...` (a non-empty cursor ⇒ the user has already scrolled past these rows once) is the one query in this codebase that actually meets that bar today; the choice is made explicitly in `listTransfersHandler` (`services/transfers-svc/http.go`), not through a global "always read from the replica" switch.

The mechanism is two separate connection pools in `transfers-svc/main.go`: `pool` (writes + anything read-your-writes-sensitive, `DATABASE_URL` → `pg-haproxy:5432`, i.e. the current leader) and `readPool` (`DATABASE_URL_REPLICA` → `pg-haproxy:5433`, i.e. any current standby, falling back to the leader if the variable isn't set — so `go test` and any environment without standbys still work, just without the benefit of them). `listTransfersHandler` picks the pool based on whether the request has a cursor — the decision is visible right in the handler, not hidden in a shared data-access layer.

What Patroni changed here: `DATABASE_URL_REPLICA` used to point at `postgres-replica-async` — a **container** that held that role permanently. That's exactly the assumption a failover breaks: the node serving those reads can end up as the leader after a switchover. Now the address describes a **role**, not a host, and HAProxy itself removes the leader from the standby pool the moment it's promoted (Patroni only answers 200 on `/replica` for a node in recovery). The flip side — the read pool can now land on the synchronous standby too, not only the asynchronous one: for the one query that goes through it, that's strictly better (fresher), and it doesn't change the rule above.

No other service is affected yet: `accounts-svc` serves the balance, `transfers-svc`'s `GET /deposits/{id}` serves a deposit's status right after payment — both are read-your-writes-sensitive by definition and stay on the leader. Giving them a read pool with nothing to read from it would be dead code, not compliance with the rule.

## Events (Kafka)
`auth-svc` publishes `UserActivated` to the `user.events` topic, `accounts-svc` publishes `AccountCreated` to `account.events`, `transfers-svc` publishes `TransferCompleted`/`TransferFailed`/`TransferRejected` to `transfer.events`. Contracts live in `proto/events/v1/{user,account,transfer}_events.proto`, serialized as binary protobuf. The message key is `user_id` for `UserActivated`/`AccountCreated`, `sender_account_id` for Transfer* events: this guarantees every event for one user/account lands in the same partition and is processed in order. `event_id` is a random UUIDv4 (`outbox.GenerateEventID`, see below), used by consumers (accounts-svc, notifications-svc) to deduplicate on redelivery (see "Idempotency" below and "notifications-svc" further down).

Besides the key and body, every message carries a **Kafka header `event_type`** (`outbox.HeaderEventType`) with the value from the outbox row's `event_type` column — literally `TransferCompleted`, `UserActivated`, etc. This is part of the wire contract, not a debugging tag: protobuf isn't self-describing, and on a topic carrying several message types (`transfer.events`) the consumer has nothing else to go on — details in the section on transfer emails.

`accounts-svc` is a consumer of the `user.events` topic (consumer group `accounts-svc`): on `UserActivated` it creates a row in `accounts` with a generated account number and `status = 'active'`, and **immediately after that** — calls `ledger-svc`'s `CreateLedgerAccount(account_id)` over gRPC, so the new account gets a ledger account (the ledger address is the `LEDGER_GRPC_ADDR` env var, default `ledger-svc:8083`). The commit ordering matters: if the ledger call fails, the event's offset is **not** committed — Kafka will redeliver the message, and idempotency (both the consumer's and `CreateLedgerAccount`'s own) makes the retry safe. This is exactly the case at-least-once + idempotency was built for.

Broker auto-creation of topics is enabled (`KAFKA_CFG_AUTO_CREATE_TOPICS_ENABLE: "true"` is set explicitly in `docker-compose.yml`, even though it's Kafka's default anyway), but we stopped relying on it: a one-shot `kafka-init` service creates all three topics with an explicit retention policy before notifications-svc starts — `compact` for `user.events`/`account.events`, `delete` for `transfer.events`. Why they differ — see "Kafka: offset reset and retention" and "`transfer.events` — `delete`, not `compact`" further down. Neither auth-svc nor transfers-svc blocks startup on Kafka's availability: the producer (`segmentio/kafka-go`) connects lazily on the first write and reconnects on its own, just like the Postgres/Redis clients.

### Outbox: how publishing survives Kafka being unavailable
Both auth-svc and transfers-svc publish events through a transactional outbox rather than directly at request time — the shared implementation (table + relay) lives in `pkg/outbox` (`neobank/pkg/outbox`), pulled in via `require`/`replace` the same way as `pkg/health` and `proto/gen/go`.

The mechanics are identical for both services:
1. The event is written to the outbox table **in the same Postgres transaction** as the business change it describes (`outbox.InsertEvent`, called with an already-open `pgx.Tx`) — either both are written or neither is: the event can't "get lost" from a crash between the business row's commit and the Kafka publish, and it can't survive a rollback that never happened.
2. A separate relay worker (`outbox.RunRelay`, a once-a-second ticker) grabs unprocessed rows (`published_at IS NULL`) via `SELECT ... FOR UPDATE SKIP LOCKED LIMIT 100`, publishes them to Kafka, and marks `published_at = now()` — the publish happens **before** the mark, so a crash between them produces a duplicate (safe: the consumer deduplicates by `event_id`), not a silent loss. `SKIP LOCKED` is there for multiple instances of the same service: each grabs its own batch without blocking on another instance's.
3. Published rows aren't deleted right away — `outbox.RunCleanupWorker` (once an hour) deletes only rows older than `OUTBOX_RETENTION` (default 7 days), keeping recent history available for debugging.

The tables are named differently in the two services (`outbox` in transfers-svc, `auth_outbox` in auth-svc) — both live in the same physical `neobank` database, and the same name would have collided.

`auth-svc` historically published `UserActivated` directly from the HTTP handler right after the commit (see the TODO that used to be in `services/auth-svc/kafka.go`) — that was a deliberate MVP limitation with a known hole (a crash between the commit and the publish lost the event silently). **Migrated to an outbox**: `verifyEmailCode` now writes the event to `auth_outbox` in the same transaction that flips `users.status` to `active`; the actual publish is now asynchronous, through the same relay as transfers-svc. As a side effect, auth-svc got a real migration runner for the first time (`services/auth-svc/migrate.go`, `MigrationsTable: "schema_migrations_auth_svc"`) — previously `users`/`verification_codes` were created by the `migrate` CLI by hand, with no tracking in the service's own code.

### Manual verification
```bash
docker compose exec kafka kafka-topics.sh --bootstrap-server localhost:9092 --list
docker compose exec kafka kafka-console-consumer.sh \
  --bootstrap-server localhost:9092 \
  --topic user.events \
  --from-beginning \
  --property print.key=true \
  --timeout-ms 10000
```
`key` prints as readable text (it's `user_id`), `value` is binary protobuf and will show up unreadable in the console — expected, not a bug.

### Idempotency

`accounts-svc` is an at-least-once consumer (writes to the DB first, then commits the offset; a crash between those two steps means Kafka redelivers the same message after a restart). Redelivery of `UserActivated` is handled at two independent, complementary levels (`handleUserActivated` in `services/accounts-svc/kafka.go`):

1. **`accounts.user_id UNIQUE`** — the INSERT uses `ON CONFLICT (user_id) DO NOTHING`. If a row for this `user_id` already exists, redelivery doesn't create a second one and doesn't fail — it's logged ("already exists... not recreating") and the offset commits as usual. This is the only level that's *mandatory*: on its own it guarantees no duplicates ever, even if something below it goes wrong.
2. **`processed_events`** (migration `000002`, `event_id UUID PRIMARY KEY, processed_at TIMESTAMPTZ`) — a fast path for already-processed events: before processing, the consumer checks whether `event_id` is in the table, and if so, skips the work entirely, without even touching `accounts`. The write to `processed_events` happens as the **last** step, strictly after the row in `accounts` is confirmed to exist (just created, or already there). This is deliberate: if the event were marked processed *before* actual processing, and processing then genuinely failed (not from a duplicate, for some other reason), the offset wouldn't commit, Kafka would redeliver — but `processed_events` would already say "done," so the retry would be falsely skipped, and the user would be left without an account forever. Writing it last closes that hole: any failure before it leaves `processed_events` empty, and a retry always genuinely reprocesses.

The two INSERTs (`accounts`, then `processed_events`) are deliberately not wrapped in a single transaction: the consumer is single-threaded and sequential (`FetchMessage` is always called one message at a time, with no concurrent processing inside the process), so there are no races between messages — and level 1 by itself makes recreating the row safe even if the write to `processed_events` never happened or was lost.

Another step wedges itself between account creation and the write to `processed_events` — the call to `ledger-svc`'s `CreateLedgerAccount(account_id)` (see above). `processed_events` is still written **last**, strictly after both the `accounts` row and the ledger account are confirmed to exist. If the ledger call fails (service unavailable, network error), the handler returns an error, the offset doesn't commit, Kafka redelivers — and `CreateLedgerAccount`'s own idempotency (`ON CONFLICT (account_id)` → returns the existing one) makes the retry safe. A cross-service RPC can't be wrapped into one SQL transaction with local writes, period — correctness of the retry is guaranteed by idempotency at every level, not by a shared transaction.

### Manually verifying idempotency

The most practical way to reproduce a redelivery without hand-assembling protobuf messages is to reset the `accounts-svc` consumer group's committed offset backward, forcing it to reread an already-processed message:

```bash
# 1. Stop accounts-svc — resetting the offset requires an inactive group
#    (Kafka considers a group active for a while after the container
#    stops, due to the session timeout; check the state with
#    --describe and wait for "has no active members"):
docker compose stop accounts-svc
docker compose exec kafka kafka-consumer-groups.sh \
  --bootstrap-server localhost:9092 --describe --group accounts-svc

# 2. Move the user.events topic's offset back by 1 message
#    (to the last processed UserActivated):
docker compose exec kafka kafka-consumer-groups.sh \
  --bootstrap-server localhost:9092 \
  --group accounts-svc --topic user.events \
  --reset-offsets --shift-by -1 --execute

# 3. Start accounts-svc again — it rereads the same message:
docker compose start accounts-svc
docker compose logs -f accounts-svc
```

Manually verified on this stack (`bitnamilegacy/kafka:3.7.1`): after step 3, the logs show `accounts-svc: event <event_id> already processed, skipping (redelivery)`, and `SELECT count(*) FROM accounts WHERE user_id = '<user_id>'` stays `1`. Level 1 was also verified separately: manually delete the row from `processed_events` (`DELETE FROM processed_events WHERE event_id = '<event_id>'`) and repeat steps 1–3, and the log shows a different branch instead — `account for user <user_id> already exists (redelivery of event <event_id>), not recreating` — meaning deduplication still fires without `processed_events`, on `ON CONFLICT (user_id)` alone; the row in `processed_events` is then restored (self-healing), and there's still just one account. Either way, the consumer's offset ends up committed (`kafka-consumer-groups.sh --describe` shows `LAG 0`), i.e. a duplicate never leaves the group "stuck."

## notifications-svc: the `user_contacts` projection built from events

Before sending any emails (that's the next section), `notifications-svc` builds and maintains its own local projection from `user_id`/`account_id` → `email` (`user_contacts`), entirely from Kafka events, with not a single synchronous call into auth-svc or accounts-svc. This is a deliberate architectural choice: a service specifically pulled off the critical path (sending emails must not block registration or transfers) shouldn't gain a dependency on another service's uptime just to find out someone's email — every service owns its own data, and notifications-svc keeps its own independent copy of what it needs.

Two consumers (one consumer group `notifications-svc`, two readers — `kafka-go`'s `Reader` subscribes to exactly one topic, so one reader per topic, not one per group):
- `user.events` → `UserActivated` → `upsertUserContactEmail` creates/updates the `(user_id, email)` row, doesn't touch `account_id`.
- `account.events` (a new topic, published by accounts-svc through the same outbox approach as transfers-svc/auth-svc — see `services/accounts-svc/accounts.go`, `tryCreateAccount`) → `AccountCreated` → `updateUserContactAccountLink` fills in `account_id` and `account_number` on the already-existing row.

`AccountCreated` always causally follows `UserActivated` (accounts-svc only creates an account in response to `UserActivated`), but the two topics have independent readers with no guarantee of relative processing order inside notifications-svc. That's why `updateUserContactAccountLink` is deliberately an `UPDATE`, not an `UPSERT`: if the `user_contacts` row doesn't exist yet (the `user.events` handler hasn't caught up), `RowsAffected = 0`. The schema's `email TEXT NOT NULL` rules out the opposite strategy (an upsert with an empty email).

The wait here happens **inside the process** (`contactWaitAttempts` × `contactWaitDelay` = 15 × 200ms), not "return an error and rely on redelivery." `kafka-go`'s `Reader` has **no** per-message redelivery within a running process: `FetchMessage` always hands back the next message regardless of whether the previous offset was committed. "Don't commit" only helps if the process restarts before any later offset on the same partition gets committed; once that's happened, the skipped message is gone. The loop's cap exists so a genuinely stuck case doesn't block the goroutine forever.

Deduplication is the same idempotent-consumer pattern as accounts-svc: check before processing, write after. Its own table, `notifications_processed_events`, not `processed_events` — that one's already taken by accounts-svc in the same physical `neobank` database (the same class of collision that already forced renaming the outbox tables to `auth_outbox`/`accounts_outbox`, see above). Every event type writes to the one table — `event_id` is globally unique regardless of type. For `UserActivated`/`AccountCreated` the status is always `skipped`: they feed the projection and never produce an email (auth-svc emails the user about registration itself). `processing`/`sent` show up on transfer events — see the next section.

### Kafka: offset reset and retention

`UserActivated` has been published since sprint 2, and `notifications-svc` is only connecting now — a new consumer group with no explicit settings could start reading `user.events` from the end of the topic, and every user registered earlier would be left out of the projection forever, with no email. The fix, both parts mandatory together, neither sufficient alone:

1. **`StartOffset: kafka.FirstOffset`** in `newKafkaReader` (`services/notifications-svc/kafka.go`) — explicit, not relying on `kafka-go`'s default. It only takes effect once: as long as the `notifications-svc` group has no committed offset on the partition. After the first offset commit this value is never read again — reading always resumes from the committed position, so it's safe to leave this setting in the code forever rather than remove it after the first deploy.
2. **`cleanup.policy=compact`** on the `user.events` and `account.events` topics (not `delete`, the broker's default) — otherwise `FirstOffset` wouldn't help if old messages were already physically removed by retention before notifications-svc ever connected. Compaction instead keeps the last message per key (`user_id`) forever — exactly what's needed to build a state projection, and natural for a topic where there's almost always one `UserActivated` per user. Applied through the one-shot `kafka-init` service in `docker-compose.yml` (`kafka-topics.sh --create --if-not-exists ... --config cleanup.policy=compact` + `kafka-configs.sh --alter --add-config` — the second one is idempotent and covers the case where the topic already existed with the default policy before this change). `notifications-svc` depends on `kafka-init` (`condition: service_completed_successfully`) — the topics are guaranteed to be configured before the first read.

Manually verified: with `notifications-svc`/the Postgres volume stopped, and `UserActivated` messages already sitting on the topic from users registered before this service existed — after `docker compose up` (migrations apply, the reader starts at `FirstOffset`) every one of them shows up in `user_contacts` with no new users created and no calls to auth-svc.

### Resilience to auth-svc being unavailable

`notifications-svc` never calls auth-svc (neither HTTP nor gRPC) — all communication is through Kafka and its own DB only. Verified: `docker compose stop auth-svc`, then the normal flow (a transfer, or a previously unprocessed `UserActivated` via an offset shift) — `notifications-svc` keeps reading and processing events with no errors, `/healthz` stays `200`.

## User profile (auth-svc)

`GET /profile` and `PATCH /profile` (through the Gateway, `X-User-Id` from the JWT — the same pattern as `accounts-svc.GET /me`) serve and update the user's `display_name`. Why this lives in auth-svc rather than a separate service — see the mini-ADR "Profile lives in auth-svc, not a separate profile-svc" above. `avatar_key`/`avatar_updated_at` are columns on `users` (migration `000005`); avatar upload itself is below, "Avatar upload."

### `display_name` validation

Trimming leading/trailing whitespace isn't an error, it's normalization. After trimming:
- **an empty string means reset to `NULL`**, not "name == empty string": a PATCH with `{"display_name": ""}` (or the field omitted entirely — the endpoint accepts exactly one field, and an omitted field simply has nothing to mean "leave as is" in this narrow request body) clears the display name;
- longer than `maxDisplayNameLength` (50 runes, not bytes — multi-byte scripts like Japanese aren't penalized for their UTF-8 encoding) — rejected;
- any control character (`unicode.IsControl` — `\n`, `\t`, a null byte, the C1 range, etc.) — rejected;
- any of the **text-direction override/isolate** characters (LRE, RLE, PDF, LRO, RLO, U+202A–U+202E, and LRI/RLI/FSI/PDI, U+2066–U+2069) — rejected. RLO in particular is the classic visual-spoofing vector (the same trick used in filename spoofing attacks): the string reads on screen in a different order than it's actually stored. Directional **marks** (LRM/RLM, U+200E/U+200F) are deliberately not on the reject list — they're rendering hints, not overrides, and can't change character order on their own.

### `ProfileUpdated`: the same outbox as `UserActivated`

`updateProfile` (`services/auth-svc/profile.go`) writes the new `display_name` value and a `ProfileUpdated` event to `auth_outbox` in one transaction — the same dual-write protection `verifyEmailCode` already applies to `UserActivated`. The event travels through the same relay, to the same `user.events` topic, with the same `partition_key = user_id` — deliberately, so Kafka guarantees ordering between `UserActivated` and any later `ProfileUpdated` for the same user with no extra waiting on the consumer side (unlike `AccountCreated`, which lives on an independent topic with no such guarantee — see "notifications-svc: the `user_contacts` projection" above).

`notifications-svc`'s `user.events` consumer (renamed to `runUserEventsConsumer`, was `runUserActivatedConsumer`) tells `UserActivated`/`ProfileUpdated` apart by the `event_type` header the relay stamps on every message — the same trick already used for the three types on `transfer.events`. A message with no header (older records) is treated as `UserActivated` — the only thing ever published to this topic before `ProfileUpdated` existed, so this isn't a hack, it's a correct reading of a legacy message. `handleProfileUpdated` is an `UPDATE`, not an `UPSERT`: the `user_contacts` row is already guaranteed to exist (it's created by `UserActivated`, which — by the partition ordering proven above — is processed first), so unlike `updateUserContactAccountLink`, no wait loop is needed here.

### Manual verification
```bash
curl -s -X PATCH http://localhost:8080/auth/profile \
  -H "Authorization: Bearer $ALICE_TOKEN" -H "Content-Type: application/json" \
  -d '{"display_name":"Alice"}'
# {"user_id":"...","display_name":"Alice","avatar_key":null,"avatar_updated_at":null}

curl -s http://localhost:8080/auth/profile -H "Authorization: Bearer $ALICE_TOKEN"
# same value — GET and PATCH use the same query against users

# A name made of control characters is rejected without touching the DB:
curl -s -o /dev/null -w "%{http_code}\n" -X PATCH http://localhost:8080/auth/profile \
  -H "Authorization: Bearer $ALICE_TOKEN" -H "Content-Type: application/json" \
  -d '{"display_name":"Alice\nBob"}'
# 400

# Reset to NULL with an empty string:
curl -s -X PATCH http://localhost:8080/auth/profile \
  -H "Authorization: Bearer $ALICE_TOKEN" -H "Content-Type: application/json" \
  -d '{"display_name":""}'
# {"user_id":"...","display_name":null,...}

# notifications-svc's projection has updated (after the relay has had
# time to deliver the event — usually under a second):
docker compose exec pg-haproxy psql -U neobank -d neobank -c \
  "SELECT user_id, display_name FROM user_contacts WHERE user_id = '<Alice's uuid>'"
```
Verified: `PATCH` saves the name (visible in the response and on a repeat `GET`), `ProfileUpdated` reaches `notifications-svc` (the row in `auth_outbox` gets `published_at`, then `user_contacts.display_name` changes), and a name containing `\n` is rejected with a `400` without a single call to Postgres.

## Avatar upload (auth-svc + MinIO)

### Principle: bytes never pass through the backend

`POST /profile/avatar/upload-url` issues a presigned PUT policy (a POST policy — the S3 term; why that and not a bare PUT, below) with a short TTL; the client uploads the file directly to MinIO. `auth-svc` never sees the upload body at this step — the same principle as with Stripe Elements ("Stripe-funded deposits" above): anything sensitive or heavy never crosses the service's perimeter, and the service doesn't hold a connection open for megabytes of upload or need to scale for image traffic.

The cost of this principle is that validation BEFORE upload is impossible: the service never saw anything until the file was already in the bucket. Hence the second endpoint.

### `POST /profile/avatar/confirm` — all validation happens here, after the fact

`confirmAvatarHandler` (`services/auth-svc/avatar.go`) fetches the object from MinIO by `key` and checks, in this order — each check cheaper than the next, so nothing pays more than it has to in order to be rejected:

1. **Key ownership.** `key` must be exactly `avatars/{X-User-Id}/{upload-id}` — generated by this same service when the presigned URL was issued, under the calling user's own prefix. The client passes `key` in the request body, so this is the only authorization check standing between "confirm your own upload" and "confirm an arbitrary key in the bucket."
2. **Real size** — `StatObject` with no body download; the limit is enforced both at the presigned-policy level (`content-length-range`, below) and here, in case the policy was ever misconfigured.
3. **Type by magic bytes.** `http.DetectContentType` on the downloaded bytes — never by the key's extension and never by the `Content-Type` the client sent: both are trivially forged, only the content isn't. The allow-list is closed and explicit (`allowedAvatarContentTypes`): `image/jpeg`, `image/png`.
4. **Decompression bomb** — `image.DecodeConfig` (reads only the format header, not the pixels) yields the claimed `width`/`height` before a buffer for full decoding is ever allocated; `width*height` above `maxAvatarPixels` (20 MP) is rejected here, before any allocation. A small, highly compressible file with enormous claimed dimensions is exactly the case this check catches; see the `TestDecodeAvatarImage_DecompressionBomb` test (`avatar_test.go`) — a solid 6000×6000 PNG weighs 4.5 KB and is rejected specifically on resolution, not file size.

A file rejected at any of these steps is **not deleted** — it stays in storage until the background cleanup (below). A `confirm` that failed partway through shouldn't get to decide whether it's safe to erase something a retry might still need.

### Re-encoding, not storing what was sent as-is

A file that passes the checks isn't saved byte-for-byte — it's decoded into an `image.Image`, center-cropped to a square (the side is the smaller of width/height), scaled to 64 and 256 px (`golang.org/x/image/draw`, `CatmullRom`), and re-encoded to JPEG. Two independent reasons, both real, not hypothetical:

- **EXIF.** A phone photo carries GPS coordinates of where it was taken. A decoded `image.Image` has no memory of the source file's byte layout — the metadata isn't "removed," there's physically nowhere for it to come from when re-encoding from scratch. Verified on a real photo with real GPS tags (Pillow — `exiftool` isn't available without admin rights in this environment, `pip install piexif Pillow` isn't either): after `confirm`, `Image.getexif()` returns an empty dict on both resulting images, the GPS IFD (`0x8825`) is entirely absent.
- **Polyglots.** A file that's simultaneously valid as an image and as something executable stops being that after re-encoding — the output is strictly JPEG, produced by `image/jpeg.Encode` from scratch, not the bytes anyone actually sent.

Both versions (64 and 256 px) are uploaded under `{key}/64` and `{key}/256`; the original upload at the bare `key` is deleted (best-effort — see `avatar.go`) right after, since only the re-encoded versions are ever served.

### A presigned POST policy, not a bare PUT — size limits enforced at the storage layer

`SetContentLengthRange` in `minio.PostPolicy` is what turns "the client sent a file bigger than it should be" into a rejection from MinIO/S3 before the bytes ever reach `confirm`, rather than a client-side `file.size` check a hostile client simply won't bother making. A bare presigned PUT carries no such policy — hence a POST policy specifically, not a PUT URL. Verified (`TestPresignedUploadURL_RejectsOversizedUpload`, `avatar_integration_test.go`): a real HTTP POST with a body larger than the limit is rejected by MinIO directly.

### Replacing an avatar deletes the previous object

`swapAvatarKey` (`avatar.go`) reads the user's current `avatar_key` (`SELECT ... FOR UPDATE`, to see the value BEFORE its own `UPDATE` — an `UPDATE ... RETURNING (SELECT ...)` in a single statement would already return the new value, not the old one) and hands it back to the calling code. After the two new objects are successfully written and `avatar_key` is updated, both objects at the old key (`{old}/64`, `{old}/256`) are deleted — otherwise every avatar change leaves garbage in the bucket forever.

### Unconfirmed uploads: background cleanup

Two independent workers, both modeled on `runResolveAttemptsCleanupWorker` in accounts-svc:

- `runAvatarUploadAttemptsCleanupWorker` (`avatar_rate_limit.go`) cleans up the attempt-tracking table itself (`avatar_upload_attempts`) — the same problem `iban_resolve_attempts` has there.
- `runAvatarCleanupWorker` (`avatar_cleanup.go`) cleans up **objects in MinIO**: once an hour it looks under the `avatars/` prefix for keys with exactly two slashes (`avatars/{user}/{id}` — an unprocessed upload; a processed one always has three segments, `avatars/{user}/{id}/{64|256}`, and is never touched) older than 24 hours, and deletes them. A successful `confirm` already deletes its own raw key right away — so an orphaned key surviving a full day means `confirm` was either never called at all, or rejected the file during validation.

### Rate limit on issuing upload URLs

Issuing a presigned URL costs the server nothing — with no limit, that's a way to endlessly spawn upload targets and fill storage with garbage, regardless of what `confirm` ultimately rejects. `recordAvatarUploadAttempt` (`avatar_rate_limit.go`) is the same atomic single-query `WITH ... COUNT ... INSERT ... SELECT ... WHERE` as `recordResolveAttempt` in accounts-svc, defaulting to 5 attempts / 10 minutes (`AVATAR_UPLOAD_RATE_LIMIT`/`AVATAR_UPLOAD_RATE_WINDOW`) — looser than the IBAN resolve limit, since changing an avatar a few times in ten minutes is ordinary behavior for a real user, not ten different resolves in a row.

### Serving: presigned GET, not public bucket reads

Both options are valid, there's no single right answer here — the task explicitly calls for picking one and justifying it. **Presigned GET** was chosen (`avatarGetURLTTL` = 1 hour, re-signed on every `GET /profile`).

**Rejected:** public bucket reads (`mc anonymous set download`) — simpler, cached by a CDN for free, no signing needed on every response.

**Why presigned, not public, specifically here:** an avatar isn't a secret (a transfer doesn't depend on who's seen it), but it is personal data tied to a specific, identifiable bank customer. A public bucket with a predictable or brute-forceable key would mean anyone, without authenticating at all, could browse the bank's customers' photos. The keys here are random UUIDs, not sequential ids, so direct enumeration isn't possible — but public reads would still mean anyone who's seen the URL ANYWHERE (a referrer header, a proxy log, sharing it) gets access forever, with no TTL and no way to revoke it. A presigned URL with an hour's TTL is the same trade-off already made for JWT tokens in this system (short-lived, not eternal), applied to one more kind of access to personal data. The cost is no CDN caching and re-signing on every `GET /profile`; at the volume of avatars in this system that's not a bottleneck worth solving now (the same principle as in "Hot account" — don't optimize what hasn't been measured as a problem).

### What broke along the way: two different endpoints for the same MinIO

The first version used the same address (`MINIO_ENDPOINT=minio:9000`, the docker-compose name) both for `auth-svc`'s own calls (Stat/Get/Put/Remove/List — these really do go out from the `auth-svc` container inside the compose network) and for signing presigned URLs. A live check immediately surfaced the problem: a presigned URL with host `minio` doesn't resolve for any client outside the docker compose network — not a browser, not `curl` from the host machine. A second symptom of the same root cause: `minio-go.PresignedPostPolicy` itself makes a network call (`GetBucketLocation`) to the address of the client it's signing with — if the "external" address (`localhost:9000`) is used for signing but the code runs inside the `auth-svc` container, that address can no longer reach MinIO at all (`localhost` inside a container is the container itself). The fix — two `minio-go` clients in `avatarStorage` (`storage.go`): `client` on `MINIO_ENDPOINT` for everything the service does itself, `publicClient` on `MINIO_PUBLIC_ENDPOINT` only for signing, plus an explicit `Region` in both clients' options so signing never tries to figure anything out over the network itself.

### Manual verification
```bash
# 1. Issue a presigned URL (rate-limited, 5/10 min per user)
curl -s -X POST http://localhost:8080/auth/profile/avatar/upload-url \
  -H "Authorization: Bearer $ALICE_TOKEN"
# {"url":"http://localhost:9000/avatars/","fields":{...},"key":"avatars/<uuid>/<uuid>"}

# 2. Actually upload the file (a POST policy, multipart/form-data — this is
#    what the frontend will do in sprint 13; for manual verification,
#    Python/requests or curl -F for each field from "fields" plus
#    -F "file=@avatar.jpg" both work)

# 3. Confirm
curl -s -X POST http://localhost:8080/auth/profile/avatar/confirm \
  -H "Authorization: Bearer $ALICE_TOKEN" -H "Content-Type: application/json" \
  -d '{"key":"avatars/<uuid>/<uuid>"}'
# {"avatar_key":"...","avatar_url_64":"http://localhost:9000/...","avatar_url_256":"..."}

# 4. A real JPEG with GPS in its EXIF (verified on a real photo with actual
#    Statue of Liberty coordinates, Pillow/piexif) — after confirm, both
#    resulting images: Image.getexif() is empty, GPS IFD (0x8825) is absent.

# 5. A file with a .jpg-looking key but text content — confirm:
# {"error":"unsupported image type \"text/plain; charset=utf-8\""} — 400,
# the object stays in MinIO (for background cleanup), avatar_key doesn't change.

# 6. A second confirm for the same user — GET /auth/profile is 200 again,
# but the old avatars/<prev-uuid>/{64,256} disappear from the bucket (mc ls).
```
Verified live (not just with unit tests): a full cycle through a real Gateway — registration, a presigned upload of a real JPEG with EXIF GPS, confirm, both resulting images downloaded via presigned GET and run through Pillow (0 EXIF tags in the output), a non-image with a `.jpg` name rejected on confirm with the object left in storage, the rate limit firing exactly when the limit was exhausted. A duplicate check via `go test` (`services/auth-svc/avatar_integration_test.go`) — the same logic against the same MinIO, including avatar replacement and cleanup of unconfirmed uploads with `retention=0` (not real hours of waiting).

## ledger-svc: internal gRPC API

`ledger-svc` computes and stores balances (`account_balances` — a cache on top of the `entries` log, always recomputable from it), executes atomic transfers between two accounts, and serves transaction history. It has **no** public HTTP API and **no** route in `gateway` — deliberately: ledger-svc's only client is `transfers-svc` (since sprint 5), which is itself responsible for authenticating and authorizing a transfer before ever calling the ledger. There's no `X-User-Id` here, nor any other client identity — this is an internal, service-to-service contract.

The protocol is gRPC, not HTTP: this is a call between services inside the cluster, not a browser request, and `buf.gen.yaml` in the repo was already set up to generate gRPC stubs (`protoc-gen-go-grpc`), so adding the contract was cheap.

The contract lives in `proto/ledger/v1/ledger.proto` (`ledger.v1.LedgerService`):
- `GetBalance(account_id) → balance` — an O(1) read from `account_balances`.
- `ExecuteTransfer(from_account_id, to_account_id, amount) → transaction_id` — an atomic transfer; business errors ("insufficient funds," "account not found" — separately for `from`/`to`, "invalid amount") come back as gRPC statuses (`FailedPrecondition`, `NotFound`, `InvalidArgument`), not as a field in a successful response — this is the gRPC-idiomatic equivalent of an HTTP status + a JSON `{"error": ...}` in the rest of the repo's services.
- `GetHistory(account_id, limit, offset) → entries[]` — paginated, newest first (`ORDER BY created_at DESC, id DESC`; `id` is the tie-breaker, because both entries of one transfer get the same `created_at`: `now()` inside a single Postgres transaction is fixed at its start).

Generating Go code from `.proto`: `buf generate` from the repo root (needs `buf`, `protoc-gen-go`, `protoc-gen-go-grpc` locally).

The server additionally registers the standard gRPC health check (`grpc.health.v1.Health`) instead of an HTTP `/healthz`, and gRPC reflection — for an internal-only service with no external consumers, the "reflection exposes the contract" trade-off doesn't apply, and reflection saves having to hand out `.proto` files just to poke the service with `grpcurl`.

### Manual verification
```bash
grpcurl -plaintext localhost:8083 list

grpcurl -plaintext -d '{"account_id": "<uuid>"}' \
  localhost:8083 ledger.v1.LedgerService/GetBalance

grpcurl -plaintext -d '{"from_account_id": "<uuid>", "to_account_id": "<uuid>", "amount": 1000}' \
  localhost:8083 ledger.v1.LedgerService/ExecuteTransfer

grpcurl -plaintext -d '{"account_id": "<uuid>", "limit": 10, "offset": 0}' \
  localhost:8083 ledger.v1.LedgerService/GetHistory
```

### Concurrency: a transfer can never push an account negative

`executeTransfer` is the only writer to `entries`/`account_balances`, and it must reject a transfer if the balance isn't enough. The danger is a classic read-then-write race: two concurrent transfers off the same account both read the same (not-yet-debited) balance, both see "sufficient funds," and both go through — the account goes negative even though each check on its own was "correct."

**`SELECT ... FOR UPDATE` was chosen, not `SERIALIZABLE`.** Both sides of the transfer (the `from` and `to` rows in `ledger_accounts`) are locked with `FOR UPDATE` inside a single transaction, **in a deterministic order — ascending `account_id`**, not `from`→`to` order. Without this, two opposing transfers (A→B and B→A at the same time) could grab their locks in opposite orders and deadlock; sorting by `account_id` guarantees both transactions always try to lock the same account first — the second one just waits, a deadlock is impossible. `SERIALIZABLE` would have solved the race too, but would have required a retry loop on `40001 serialization_failure` — no such pattern exists anywhere else in the repo, and introducing it for one function alone would mean a new, unsupported class of errors. `FOR UPDATE` instead simply blocks the second transaction until the first commits — the same trick already used in `accounts-svc` (`updateAccountStatus`) and `auth-svc`, just locking two accounts here instead of one.

**The test that proves it** — `TestExecuteTransfer_ConcurrentOverdraftPrevention` (`services/ledger-svc/ledger_test.go`): an account with a 10000 balance, 20 goroutines simultaneously trying to debit 1000 each (20000 total — twice what's actually there). Expected: exactly 10 successes, 10 `insufficient funds`, a final balance of exactly 0 (never negative), and `SUM(entries)` across every account involved in the test equal to 0.

This isn't a "one check at a time" logic test — it genuinely spins up 20 goroutines in parallel, so the race, if it exists, has a real chance to show up. Manually verified: temporarily removing `FOR UPDATE` from `lockLedgerAccount` makes the test fail reliably (10 out of 10 runs) — all 20 transfers go through, the balance ends up at −10000. With `FOR UPDATE`, the test is reliably green (run with `-count=15` in a row). Run it yourself:
```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go test ./... -run TestExecuteTransfer_ConcurrentOverdraftPrevention -count=20 -v
```
(`-race` doesn't apply here — it catches Go memory races, not Postgres row-lock races, which is exactly what's being tested; the goroutine in the test itself has no shared mutable state — each one only writes to its own slice index.)

## accounts-svc: the balance in `GET /accounts/me`

`GET /accounts/me` (through the Gateway, with a valid token) returns the user's account **together with the balance**. The balance is authoritative and lives in `ledger-svc`; accounts-svc gets it by calling `GetBalance(account_id)` over gRPC (`account_id` = `accounts.id` = `ledger_accounts.account_id`).

Format: `balance` is an integer in **minor units** (cents), plus a separate `currency` field (currently always `"EUR"` — the ledger has no notion of currency, and formatting `"€123.45"` is the frontend's job, not the API's):
```json
{ "id": "...", "user_id": "...", "account_number": "NB...", "iban": "IE34ZZZZ00004234567890",
  "status": "active", "created_at": "...", "updated_at": "...", "balance": 50000, "currency": "EUR" }
```
A brand-new user already has a ledger account (created by the Kafka handler above), no entries yet → `balance: 0`.

If `ledger-svc` is temporarily unavailable (`Unavailable`/`DeadlineExceeded`), the endpoint returns **503**, not a `200` with a zero balance: showing a fake zero instead of the real balance in a bank is worse than honestly saying "service unavailable."

## Dev tools

> Local development only. Not the path to production.

- `services/ledger-svc/cmd/seed` — fills the local DB with sample ledger data (genesis + two accounts, see the file header).
- `services/ledger-svc/cmd/devtopup` — **topping up a user's account** before Stripe existed (sprint 9). Transfers `--amount` cents from the genesis account to `--account-id` (that's `accounts.id`) through ledger-svc's **ordinary `ExecuteTransfer`** — the same real path (locks, balance check, cache update) as a production transfer. The one thing that can't go through `ExecuteTransfer` is minting money (the source would go negative, and `ExecuteTransfer` forbids that): so when genesis doesn't have enough funds, the tool **mints** money into genesis with a direct, balanced DB insert (external → genesis, so `SUM(entries)=0` is preserved), and only then makes the real transfer. That direct minting is exactly why this is a dev tool, not an HTTP endpoint.

  ```bash
  # from services/ledger-svc, ledger-svc must be running (docker compose up ledger-svc)
  DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  LEDGER_GRPC_ADDR="localhost:8083" \
    go run ./cmd/devtopup --account-id <accounts.id> --amount 50000
  ```
  After this, `GET /accounts/me` for the same user shows `"balance": 50000`.

## Moving money through the ledger (transfers-svc)

`transfers-svc` creates a `transfers` row in `pending` status (validation: the amount is positive, the recipient resolves by IBAN via `accounts-svc.ResolveAccountByIban` — for resolve details and its failure modes see "Resolving a recipient by IBAN" below, recipient ≠ sender, recipient isn't `closed`, sender is `active` — the balance is **not** checked here, `ledger-svc` does that atomically), then calls `ledger-svc.ExecuteTransfer(sender_account_id, recipient_account_id, amount)` and updates the row based on the result:
- success → `status = 'completed'`, `ledger_transaction_id` — the entry id from the ledger's response.
- `ledger` returned "insufficient funds" (`FailedPrecondition`) → `status = 'failed'`, `failure_reason = 'insufficient_funds'`.
- `ledger` returned "account not found" (`NotFound`) or an invalid amount (`InvalidArgument`, unreachable in practice — `transfers-svc` already checked the amount earlier) or its own internal error (`Internal`) → `status = 'failed'` with the matching `failure_reason` (`account_not_found` / `invalid_amount` / `ledger_internal_error`) — every domain error is tagged explicitly, not dumped into one generic `'error'`.

### An honest boundary: an undetermined outcome

`ledger-svc.ExecuteTransfer` (`services/ledger-svc/ledger.go`) wraps its work in a single Postgres transaction that either commits entirely or rolls back entirely (`defer tx.Rollback(ctx)`, `tx.Commit(ctx)` only on success) **before** any gRPC response goes out. That means: any well-formed response — success, or one of the explicit `status.Error(...)` values `ledger-svc` returns itself (`FailedPrecondition`, `NotFound`, `InvalidArgument`, `Internal`) — states with complete certainty whether the money moved or not.

`codes.Unavailable`, `codes.DeadlineExceeded`, and `codes.Unknown` are a fundamentally different case: `ledger-svc` itself never returns them — they only arise at the transport level (the call never got through, or no response arrived within `ledgerCallTimeout` = 5 seconds). Here `transfers-svc` genuinely **doesn't know** whether the transfer executed or not: the request might never have arrived, or it might have arrived, executed, and had its response lost on the way back. Marking it `failed` in this case would be a lie if the money really did move; marking it `completed` with no `transaction_id` would be a lie the other way. So the row stays `pending` as it already was (no DB write happens at all), and the client gets back `202 Accepted` with the body `{"status": "pending", "message": "transfer status unknown, still processing"}`.

This uncertainty resolves itself automatically — see "Reconciliation: closing out pending transfers" below. The same "stuck pending" applies to an undetermined fraud-check outcome too (see "fraud check before the ledger") — the same class of problem at two points in the flow, closed by the same reconciliation job rather than handled separately for each case.

### Manual verification
```bash
# through the Gateway: the sender resolves from X-User-Id, which the
# Gateway itself sets from the JWT after /auth/login — see the
# "API client"/JWT middleware section above
curl -s -X POST http://localhost:8080/transfers/ \
  -H "Authorization: Bearer <access_token>" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: <uuid>" \
  -d '{"recipient_iban":"IE34ZZZZ00004234567890","amount":1000}'
```
A successful transfer — `201` and `"status":"completed"` with `ledger_transaction_id` filled in; an amount larger than the balance — `201` and `"status":"failed","failure_reason":"insufficient_funds"`; both cases are verified via the balance diff through `GET /accounts/me` on both sides (see `cmd/devtopup` above — handy for giving the sender a starting balance).

## Resolving a recipient by IBAN (transfers-svc → accounts-svc)

`accounts-svc.ResolveAccountByIban` (`services/accounts-svc/grpc_server.go`) is the one place in the system that answers "does an account with this IBAN exist." That's exactly why its contract, and what it's able to tell apart, matters more than it might seem to for a single internal RPC.

### IBAN instead of `account_number` — one identifier, not two

`POST /transfers` used to accept `recipient_account_number`; now it only accepts `recipient_iban`, and `account_number` as a way to address someone else's account has been **removed**, not left as a second "for compatibility" path (`ResolveAccountByNumber` and `getAccountByAccountNumber` are physically gone from the codebase, not just marked deprecated). The reason isn't technical — both options worked equally reliably — it's product: a user shouldn't have to decide which of two numbers to give someone else to identify themselves. `account_number` hasn't gone away as a field (it's still in `GET /accounts/me`, it's the internal account number), but addressing *someone else's* account is now only possible by IBAN — a single user-facing identifier that also happens to pass a real check-digit validation.

### Three distinct outcomes — three distinct messages, not one generic 404

"Recipient doesn't resolve" used to mean one thing: `NotFound`. IBAN failures come in more flavors, and they mean different things, so `ResolveAccountByIban` tells them apart with distinct gRPC codes, in increasing order of check cost:

1. **`codes.InvalidArgument` — the format or check digits don't match.** Checked locally (`iban.Validate`), **before** any database call — not just faster, it's also part of the defense against enumeration (mod-97-10 filters out ~96% of random strings for free, see below).
2. **`codes.FailedPrecondition` — the IBAN is valid, but the bank code belongs to someone else.** No SEPA rails are wired up here at all (see "Honest limitations"), so this isn't "not found," it's "not a possible recipient at all" — a different message to the client, different diagnostics on the operator's side.
3. **`codes.NotFound` — our bank, but no such account.**
4. **`codes.ResourceExhausted` — the resolve itself is rate-limited** (below) — also its own distinct code, not a generic 500.

`transfers-svc` (`createTransfer`, `services/transfers-svc/transfer.go`) translates each of the four codes into its own `createTransferOutcome`, and `http.go` translates that into its own HTTP status and text: `400 invalid IBAN`, `400 only transfers within this bank are supported`, `404 recipient not found`, `429 too many resolve attempts, try again later`.

### Rate limit on the resolve, not just on the transfer

An endpoint that answers "exists/doesn't exist" is an oracle for enumerating someone else's bank details, and valid check digits **narrow** the search space (only strings compatible with mod-97-10 remain) rather than making enumeration safe. That's why the limit sits specifically on the resolve, per user (`user_id` — a mandatory field on `ResolveAccountByIbanRequest`, passed through from `X-User-Id`, not something a client could forge), not only on creating a transfer: a transfer carries a side effect (idempotency, fraud check, a DB write) and is naturally more expensive to hammer, while a bare resolve isn't.

Implemented in `services/accounts-svc/rate_limit.go`: a single SQL statement (`WITH ... SELECT count(*) ... INSERT ... SELECT ... WHERE count < limit RETURNING id`) counts the user's attempts within the window and, only if the limit isn't exhausted, atomically records this attempt right then — with no separate `SELECT`, and therefore no race window where two parallel requests from the same user could both see "limit not exhausted" before either one had recorded itself. Defaults to 10 attempts / 5 minutes (`IBAN_RESOLVE_RATE_LIMIT` / `IBAN_RESOLVE_RATE_WINDOW`), the `iban_resolve_attempts` table is cleaned up by a background worker once every 10 minutes (retention 1 hour — comfortably longer than the longest reasonable window).

The order of checks inside `ResolveAccountByIban` isn't arbitrary: first the free check-digit validation (no DB), then the rate limit (the only place that writes to the DB before the actual lookup), then a free bank-code comparison, and only at the end the actual `SELECT` against `accounts`. Every cheaper check runs before a more expensive one.

### Whether to show the recipient's name — we don't

Real banks show the account owner's name on the transfer confirmation screen — that's protection against a typo in the details. But that same resolve, the one that would look up the name, turns into a way to find out someone's name from their IBAN, and an IBAN isn't a secret (it ends up on a receipt, in an email, in conversation). Three options were on the table: don't show it at all; show it partially (first initial + surname); show it in full, behind a hard rate limit.

The first was chosen — not because it's the one objectively correct answer (there isn't one here, this is a deliberate trade-off, not a ready-made rule), but because in this system the decision is already made one level down: **a user's name doesn't exist anywhere** — registration (`auth-svc/register.go`) only ever collects an email and a password, and there's no name field in any table or any proto message. Showing a partial or full name would mean first adding the collection of it (a new field on the registration form, a new column, a new proto field threaded through `ResolveAccountByIban`) — and that's no longer a "how to show it" decision, it's a separate feature with its own cost (what if two users give the same name at registration? how does this square with the KYC that's also absent — see "Honest limitations"?). Showing nothing is the only one of the three options that doesn't require inventing data the system never had.

### Manual verification
```bash
# 1. A malformed IBAN — rejected without touching the DB (400, "invalid IBAN")
curl -s -X POST http://localhost:8080/transfers/ \
  -H "Authorization: Bearer <access_token>" -H "Content-Type: application/json" \
  -H "Idempotency-Key: <uuid>" \
  -d '{"recipient_iban":"IE00ZZZZ00004234567890","amount":1000}'

# 2. Another bank — a distinct message (400, "only transfers within this bank are supported")
curl -s -X POST http://localhost:8080/transfers/ \
  -H "Authorization: Bearer <access_token>" -H "Content-Type: application/json" \
  -H "Idempotency-Key: <uuid>" \
  -d '{"recipient_iban":"IE29AIBK93115212345678","amount":1000}'

# 3. Rate limit: 11 resolves in a row from one user in under 5 minutes —
#    the 11th gets 429 "too many resolve attempts, try again later"
for i in $(seq 1 11); do
  curl -s -o /dev/null -w "%{http_code}\n" -X POST http://localhost:8080/transfers/ \
    -H "Authorization: Bearer <access_token>" -H "Content-Type: application/json" \
    -H "Idempotency-Key: $(uuidgen)" \
    -d '{"recipient_iban":"IE29ZZZZ00000000000000","amount":1000}'
done
```

## Stripe-funded deposits (transfers-svc, ledger-svc)

Stripe deposits, in full: `ledger-svc.Deposit`/`ReverseDeposit`,
`POST /deposits` (a Stripe `PaymentIntent`), `POST /webhooks/stripe`
(confirmation), the background succeeded → credited posting, reconciliation
for three kinds of stuck deposits, reversal entries for refunds
(`charge.refunded`), simulated withdrawals
(`POST /withdrawals`), routing `/deposits`/`/webhooks/stripe` through the
Gateway (see "Gateway: routing" below), the frontend's `/deposit` screen
(see "Frontend: the deposit screen" below), and a unified operation history
(see "Operation history" below). `POST /withdrawals` was deliberately not
exposed externally through the Gateway — the simulated withdrawal has no
creation form on the frontend (not part of this step), and there's no point
exposing an endpoint with not a single client actually calling it.

### Why deposits live in transfers-svc, not a new payments-svc

A deposit is the same class of problem as a transfer: money movement with
uncertainty on an external system's side (there it's `ledger-svc`, here
it's Stripe) that needs to be waited on and reconciled, not guaranteed known
synchronously at the moment of the HTTP response. transfers-svc already
carries all the infrastructure this needs — a `ledger-svc` client, an
outbox for reliable event publishing, a reconciliation worker for stuck
states (see the sections above and below). Standing up a separate
payments-svc would mean duplicating all of that again for a separation of
responsibility that doesn't pay for itself at MVP scale; if/when deposits
grow their own complexity (refunds, multiple providers), splitting them
into their own service is a reasonable next step, just not now.

### Schema (`services/transfers-svc/migrations/000006`, `000007`)

`deposits` is one row per deposit attempt; `stripe_payment_intent_id` is
unique — a natural safeguard against two rows for the same PaymentIntent.
The `succeeded` and `credited` statuses are **deliberately distinct**:
`succeeded` means "Stripe confirmed the card charge," `credited` means "we
posted the ledger entry." These are two facts in two different systems;
collapsing them into one status would mean losing the recoverable state of
"Stripe already took the money, and we haven't credited it yet" — exactly
the spot a future webhook handler will have something to fix.

`processed_stripe_events` gives idempotency at the level of Stripe's
`event_id` (`evt_...`): a `PRIMARY KEY` on `event_id` is more reliable than
counting on Stripe never sending the same webhook twice.

### `ledger-svc.Deposit` / `ReverseDeposit` — genesis ↔ a user's account

`Deposit(account_id, amount, reference)` (`proto/ledger/v1/ledger.proto`):
a genesis entry, atomically debiting `amount` from the system's genesis
account and crediting it to `account_id` through the same mechanics as
`ExecuteTransfer` (one Postgres transaction, a balanced pair of `entries`
sharing a `transaction_id`, an incremental update to `account_balances`).
`ReverseDeposit(account_id, amount, reference)` mirrors it in the opposite
direction (`account_id` → genesis), for refunds (see below). Both are
**separate functions**, wrappers over a shared
`postUncheckedTransfer(from, to, amount, reference)` in `ledger.go`, not
branches inside `executeTransfer`: the one behavioral difference is
**there's no sufficient-funds check** on the side that goes negative
(genesis — by definition it has to be able to do this, representing money
entering the system from outside; for `ReverseDeposit` it's the user's
account, explained below). Baking this exception into `executeTransfer`
would mean adding a special case to the function every ordinary transfer
depends on — the shared wrapper reuses the same `lockLedgerAccount`/
`applyBalanceDelta` as `executeTransfer`, but leaves `executeTransfer`
itself completely untouched.

**Idempotent by `reference` — unlike `ExecuteTransfer`.**
`reference` is the future `deposits.id` (a UUID) for `Deposit`, or a value
deterministically derived from it for `ReverseDeposit` (see
`reversalReference` below). A repeat call with an already-used `reference`
returns the existing entry rather than creating a second one. This is
deliberately different behavior from `ExecuteTransfer` (transfers remain
non-idempotent; idempotency there lives at the `transfers.
idempotency_key` level in transfers-svc, a separate layer above): a deposit
and its reversal have no such layer, and the calling side (the background
crediting worker, a redelivered `charge.refunded` webhook) can naturally
end up calling them more than once for the same logical operation.
Implemented via `pg_advisory_xact_lock(hashtext(reference))` inside the
transaction: it serializes concurrent calls sharing one `reference`, then
checks `SELECT transaction_id FROM entries WHERE reference = $1` and either
returns what it found or posts as usual. A plain `UNIQUE` on
`entries.reference` wouldn't have worked — `executeTransfer` already
legitimately writes two rows (debit and credit) with the same `reference`
for one entry.

The system's genesis account (`00000000-0000-0000-0000-000000000001`) is
now deterministically created by the migration
`services/ledger-svc/migrations/000005_create_genesis_ledger_account` —
before this step it only ever existed as a side effect of the first run of
`cmd/seed`/`cmd/devtopup`. Neither dev tool was touched: their own
idempotent upserts of the same genesis account remain harmless no-ops now
that the migration creates the row first.

### The Stripe secret: environment variable only

`STRIPE_SECRET_KEY` (`sk_test_...`) is read once at transfers-svc startup
(`main.go`) via `os.Getenv`, the same pattern as `JWT_SECRET` in auth-svc —
the process stops (`log.Fatal`) if the variable isn't set, before
connecting to Postgres and before migrations. Unlike `JWT_SECRET`, whose
dev value is just a hardcoded string in `docker-compose.yml` (safe for an
internal secret), a real Stripe key is a far more sensitive third-party
secret: `docker-compose.yml` pulls it from `${STRIPE_SECRET_KEY}` (docker
compose itself substitutes values from `.env` at the repo root, next to
`docker-compose.yml` — this works with not one line of code), and `.env` is
in `.gitignore`; the repo only ships `.env.example` with a placeholder. The
publishable key (`pk_test_...`) isn't part of this — it's frontend-only and
deferred to the matching step.

The client is `github.com/stripe/stripe-go/v86`, created once in `main()`
via `stripe.NewClient(stripeSecretKey)` and held in a package-level (not
local) `var stripeClient *stripe.Client`. `stripe.NewClient` does nothing
over the network — like this `main()`'s other clients (`grpc.NewClient` to
accounts-svc/fraud-svc/ledger-svc), it's lazy: if the key turns out to be
invalid, that only surfaces on the first real call to the Stripe API.

### `POST /deposits` — creating the PaymentIntent

How the payment works: a `PaymentIntent` is a Stripe-side object
representing an intent to charge money. transfers-svc creates it and gets
back a `client_secret`; that secret goes to the frontend, where Stripe.js
confirms the payment **directly with Stripe** — card details never pass
through the backend at all. This isn't an implementation detail, it's a
deliberate PCI-scope boundary: the service never stores, logs, or sees a
card number. The result arrives via a webhook (the next step), not in the
response to this request.

`createDepositHandler` (`services/transfers-svc/http.go`), registered as
`POST /deposits`:
- `account_id` comes from `X-User-Id` (the same `resolveSenderAccountID` as
  transfers use) — never from the request body, so a client can't deposit
  into someone else's account.
- `amount` is checked against `[depositMinAmount, depositMaxAmount]`
  (`services/transfers-svc/deposit.go`): the lower bound is 50 (Stripe's
  minimum for EUR, €0.50 — otherwise Stripe would reject the request itself,
  just less informatively), the upper bound is 1,000,000 cents (€10,000) —
  not a business limit, protection against a typo adding extra zeros.
- the account must be `active` — nothing is credited to a `frozen`/`closed` one.

The order of operations in `createDeposit`: first `INSERT INTO deposits`
(`status='pending'`), then the Stripe `PaymentIntent`, then `UPDATE ...
SET stripe_payment_intent_id`. These two steps fundamentally can't be
wrapped in one transaction — a live call to the Stripe API sits between
them, and it can't participate in a Postgres transaction. If the process
crashes between the steps, a `pending` deposit with no
`stripe_payment_intent_id` is left behind: this is safe (no money moved,
and Stripe never even received the request in most such failures) — an
abandoned attempt, not something that needs fixing right here.

`Metadata: {deposit_id, account_id}` on the `PaymentIntent` itself is what
ties a future webhook back to this row: Stripe has no notion of our primary
keys beyond whatever we put there ourselves.
`IdempotencyKey = deposit_id` (via `stripe-go`'s `Params.IdempotencyKey`)
ties Stripe-side duplicate protection to a specific attempt: a repeat call
after a network failure) returns the same `PaymentIntent` instead of creating a second one.

The response is just `{deposit_id, client_secret}`. The Stripe secret key never leaves `main()` under any circumstances, and the only field that leaves the `PaymentIntent` object itself is `client_secret`.

### `POST /deposits` tests

`services/transfers-svc/deposit_test.go`, via `fakePaymentIntentCreator`
(the same fake-by-function pattern as `fakeLedgerClient`/
`fakeAccountsClient` — no real calls to Stripe happen in tests):
`TestCreateDeposit_Success` (including `IdempotencyKey`/`metadata`
reaching Stripe correctly), `TestCreateDeposit_InvalidAmount` (both
bounds), `TestCreateDeposit_AccountNotActive`,
`TestCreateDeposit_StripeErrorLeavesRowPending` (verifies exactly the safe
state described above).
```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go test ./services/transfers-svc/... -run TestCreateDeposit -v
```
Manual check (needs a real `STRIPE_SECRET_KEY` in `.env` — see above):
```bash
curl -s -X POST http://localhost:8084/deposits \
  -H "X-User-Id: <account's user id>" \
  -H "Content-Type: application/json" \
  -d '{"amount": 5000}'
```
`201` with `{"deposit_id": "...", "client_secret": "pi_..._secret_..."}`;
in the Stripe dashboard (test mode) — a created `PaymentIntent` with
`metadata.deposit_id`/`metadata.account_id`.

### Manual verification

```bash
grpcurl -plaintext -d '{"account_id": "<uuid>", "amount": 1000}' \
  localhost:8083 ledger.v1.LedgerService/Deposit
```
`account_id`'s balance (`GET /accounts/me` through the Gateway) grows by
exactly `amount`; genesis (`00000000-0000-0000-0000-000000000001`) goes
negative by exactly the same amount — as intended.

`STRIPE_SECRET_KEY` really is mandatory: `docker compose up
transfers-svc` with no `.env` (or with the variable commented out) — the
container fails immediately with the log `transfers-svc:
STRIPE_SECRET_KEY environment variable is required`, never even reaching
the Postgres connection.

### `ledger.Deposit` / `ReverseDeposit` tests

`services/ledger-svc/ledger_test.go`: `TestDeposit_Success` (the target
account grows by `amount`, genesis drops by `amount`, `SUM(entries)` for
the transaction is 0), `TestDeposit_InvalidAmount`,
`TestDeposit_AccountNotFound`, `TestDeposit_WithReference` (`reference` is
found via `getTransactionByReference`),
`TestDeposit_IsIdempotentByReference` (two calls with the same
`reference` — one entry, not two),
`TestDeposit_ConcurrentSameReferenceDoesNotDoublePost` (20 concurrent
calls with the same `reference` — exactly one entry; proves
`pg_advisory_xact_lock` genuinely serializes the race rather than just
looking correct under sequential calls), `TestReverseDeposit_
Success`, `TestReverseDeposit_AllowsNegativeBalance` (no balance check —
mirroring genesis's lack of a check in `Deposit`),
`TestReverseDeposit_IsIdempotentByReference`.
```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go test ./services/ledger-svc/... -run "TestDeposit|TestReverseDeposit" -v
```

### `POST /webhooks/stripe` — confirming the payment

The frontend's response after paying guarantees nothing: a user can close
the tab right after the money was charged, and that never reaches the
backend at all. The one reliable source of truth for the outcome is
Stripe's webhook, which arrives asynchronously and independently of
whether the client is even still around. `POST /webhooks/stripe`
(`services/transfers-svc/webhook.go`) is a public endpoint: Stripe has no
JWT of ours, so verifying the signature isn't optional — it's the one
thing telling a real webhook apart from anyone who guessed the URL and
sent `{"type":"payment_intent.succeeded",...}` themselves to get money
credited for free.

**Signature.** `webhook.ConstructEvent(payload, header, webhookSecret)`
from `stripe-go` — the secret for this (`whsec_...`) is separate from
`STRIPE_SECRET_KEY`, deliberately not the same one: compromising one
shouldn't automatically compromise the other. Critically, the signature is
computed over the **raw body bytes**, so `io.ReadAll(r.Body)` happens
first, before any JSON parsing: if the body were parsed and then
re-serialized (a different field order, different whitespace — doesn't
matter which), the signature would stop matching. An invalid signature
gets `400`, with no processing at all, just a log of the attempt.

**Idempotency.** Stripe delivers webhooks at-least-once and retries
undelivered ones — the same `evt_...` can arrive more than once.
Deduplication is `INSERT event_id` into `processed_stripe_events`; on a
unique violation, it's a duplicate, `200` with no reprocessing. The
arbiter is the DB constraint itself, not a `SELECT exists(...)` in code:
two webhooks with the same `event_id` can arrive in parallel, and only a
constraint correctly resolves that race (a check-then-insert in code
doesn't catch it).

An important nuance that isn't in the literal task description, but shows
up in practice: the `INSERT` into `processed_stripe_events` and the update
to `deposits` run **in a single Postgres transaction**
(`processStripeEvent`). If the event were marked processed BEFORE its
effect (the status update) was actually applied, a transient failure
between those two steps would mean: a `500` response (asking Stripe to
retry), but a genuine retry of the same delivery would then never work —
it would land in the "duplicate, ignore" branch, and the needed update
would never happen. One transaction for both steps is the only way
`400`/duplicate/failure behave exactly as described in the DoD: a failure
during processing rolls back the `processed_stripe_events` insert too, so
the next delivery of the same `event_id` is a genuinely first attempt, not
a wrongly ignored duplicate.

**Events.** Three types are handled: `payment_intent.succeeded` →
`deposits.status = 'succeeded'`; `payment_intent.payment_failed` →
`status = 'failed'` + `failure_reason` (Stripe's error code, e.g.
`card_declined`); `charge.refunded` → **doesn't change status** (`deposits`
has no separate `refunded` status — a full reversal, including a possible
schema extension, is deferred to the matching step), and instead writes
`failure_reason = 'refunded'` as a marker for future reconciliation —
reusing the field by context, the same trick `Transfer` already uses
(`FailureReason` already carries a different meaning depending on `Status`,
see above). Any other event type — `200` and ignored with no processing:
Stripe sends dozens of event types, and failing on an unfamiliar one would
make it retry forever for no benefit.

**Speed.** The handler deliberately does nothing beyond verifying the
signature, deduplicating, and one `UPDATE` — crediting `ledger-svc` (a
slower, cross-service call) isn't part of this and moves to a separate
step. Stripe has a timeout on the webhook response; a long synchronous
handling step here would mean spurious retries from Stripe on top of a
payment that already happened.

### Local webhook development: the Stripe CLI

Stripe can't reach `localhost` directly. Locally, the
[Stripe CLI](https://docs.stripe.com/stripe-cli) solves this:

```bash
stripe login
stripe listen --forward-to localhost:8080/webhooks/stripe
```

Port `8080` is the Gateway, not `transfers-svc` directly: `/webhooks/stripe`
is now proxied through the Gateway (see "Gateway" below), publicly and with
no JWT check (Stripe can't send a token — the webhook's own signature is
itself the authentication), with the request body reaching `transfers-svc`
byte-for-byte. `localhost:8084/webhooks/stripe` (the service directly,
bypassing the Gateway) still works and is handy for low-level checks of the
handler itself (see "Manual verification" below), but doesn't reflect the
real path a request from Stripe takes in production.

`stripe listen` keeps a tunnel open and prints a **local** webhook signing
secret (`whsec_...`, separate from the production/dashboard one) — that's
the value to put in `.env` as `STRIPE_WEBHOOK_SECRET`. As long as `stripe
listen` isn't restarted, the value stays the same.

With `transfers-svc` and `stripe listen` running in another terminal:
```bash
stripe trigger payment_intent.succeeded
```
sends a real, signed event to the forwarding URL — `stripe listen`'s log
shows the response code (`200`), and the DB shows an updated
`deposits.status`.

### Manual verification

`STRIPE_WEBHOOK_SECRET` is mandatory the same way `STRIPE_SECRET_KEY` is:
`docker compose up transfers-svc` with it missing from `.env` — the
container fails with the log `transfers-svc: STRIPE_WEBHOOK_SECRET
environment variable is required`, before even connecting to Postgres.

A forged signature:
```bash
curl -s -o /dev/null -w "%{http_code}\n" -X POST http://localhost:8084/webhooks/stripe \
  -H "Stripe-Signature: t=0,v1=deadbeef" \
  -d '{"type":"payment_intent.succeeded"}'
```
`400`, no row appears in `processed_stripe_events`.

### `POST /webhooks/stripe` tests

`services/transfers-svc/webhook_test.go` — via
`webhook.GenerateTestSignedPayload` (from `stripe-go/webhook`), i.e. with a
real HMAC signature computed, with not one real call to Stripe:
`TestStripeWebhookHandler_PaymentIntentSucceeded`,
`TestStripeWebhookHandler_PaymentIntentPaymentFailed`,
`TestStripeWebhookHandler_ChargeRefunded_CreditedDeposit`/
`_SucceededNotYetCreditedDeposit`/`_PendingDepositIsIgnored` (see
"Refunds" below), `TestStripeWebhookHandler_UnknownEventTypeIsIgnored`,
`TestStripeWebhookHandler_InvalidSignature` (checks both the `400` and the
absence of a row in `processed_stripe_events`),
`TestStripeWebhookHandler_DuplicateDeliveryIsNotReprocessed` (a second
delivery of the same `event_id` — `200`, but `deposits.updated_at` doesn't
change and `processed_stripe_events` still has exactly one row),
`TestProcessStripeEvent_ProcessingFailureDoesNotRecordEvent` (proves
exactly the single-transaction-for-both-steps behavior described above).
```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go test ./services/transfers-svc/... -run "TestStripeWebhook|TestProcessStripeEvent" -v
```

### Crediting: `succeeded` → `credited` — why a background worker, not the webhook handler or a task queue

The webhook only takes a deposit as far as `succeeded` — Stripe confirmed
the card was charged. The money hasn't been credited in the ledger yet;
this whole saga lives between those two facts, and `succeeded`/`credited`
are kept separate precisely so that state is visible, not hidden.

Crediting (`ledger-svc.Deposit`) deliberately doesn't happen inside
`stripeWebhookHandler`: Stripe has a timeout on the webhook response, and
a cross-service call to `ledger-svc` isn't something worth holding up the
`200 OK` for — that response is supposed to go out fast anyway (signature
check, deduplication, one `UPDATE` — see above).

The choice was between two options: a background worker polling
`succeeded` deposits, or an async job queued by the handler (which would
have required a task queue — infrastructure that simply doesn't exist in
this repo). The worker was chosen:
- It matches a pattern this service already has — `transfers-svc` already
  runs `runReconciliationWorker` (`reconcile.go`, a tick every 30s) for
  exactly this class of task: "reread the source of truth and bring local
  state in line."
- It needs no new infrastructure (a task queue, another Kafka topic for
  "go credit this") for a single new consumer.
- It retries naturally: a poller, not a deliver-once event — if crediting
  fails on this tick, it simply retries on the next one, with no separate
  retry/DLQ logic that a task queue would already have needed.

Technically: `creditSucceededDeposits`
(`services/transfers-svc/deposit_reconcile.go`) is now called on
**every** tick of that same worker, alongside the existing `reconcileOnce`
(transfers) — the same ticker, the same process, two independent concerns.
This is literally "extending the existing worker," not a new worker
alongside it. For every `succeeded` deposit: `ledger.Deposit(account_id,
amount, reference=deposit_id)` (idempotent — see above, so it's safe to
call on every tick for the same deposit until it becomes `credited`), then
`markDepositCreditedIfSucceeded` — `UPDATE ... WHERE status = 'succeeded'`
+ writing `DepositCredited` to the outbox, in one transaction
(`services/transfers-svc/deposit.go`), the same pattern as
`markTransferCompletedIfPending` for transfers. The `WHERE status =
'succeeded'` condition is the same protection against a race with a
concurrent call (another transfers-svc replica, the same tick) as
transfers have: the loser simply finds no row to update
(`RowsAffected() == 0`) and silently backs off.

### `DepositCredited` — an outbox event and an email

`UPDATE deposits ... credited` and `INSERT INTO outbox (...,
'DepositCredited', ...)` — in one transaction, the same outbox pattern
already used for `TransferCompleted`/`Failed`/`Rejected` (see "Outbox"
above). The event contract is
`proto/events/v1/deposit_events.proto`.

**A deliberate decision: `DepositCredited` travels on the same
`transfer.events` topic**, through the same `outbox` table, not a separate
`deposit.events`. A deposit is the same class of event as a transfer
(money confirmed to have moved), and `transfer.events` already has a
mechanism for multiplexing several message types via the `event_type`
header — added specifically for this (see the section on the
`event_type` header above). Standing up a second physical outbox table, a
second Kafka topic, a second relay/cleanup worker, and a second consumer
in notifications-svc for one additional event type isn't justified at MVP
scale; the same choice already made for the deposit infrastructure as a
whole (see "Why deposits live in transfers-svc" above).

notifications-svc is subscribed to this same `transfer.events` (no new
consumer was ever stood up): `eventTypeDepositCredited = "DepositCredited"`
was added to the existing `processTransferMessage` switch
(`services/notifications-svc/kafka.go`), `handleDepositCredited` has the
same shape as `handleTransferFailed` (one recipient, one email,
`claimEvent`/`finishTransferEvent` for idempotency), `buildDepositCreditedEmail`
(`email.go`) — an "account topped up" email, **sent only on `credited`**,
never earlier.

### Tests

`services/transfers-svc/deposit_reconcile_test.go`:
`TestCreditSucceededDeposits_Success` (status changes to `credited`,
`ledger_transaction_id` is filled in, exactly one `DepositCredited` row in
the outbox), `TestCreditSucceededDeposits_LedgerErrorLeavesSucceeded` (a
ledger-svc failure doesn't corrupt the deposit — it just stays
`succeeded` for the next tick).
```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go test ./services/transfers-svc/... -run TestCreditSucceeded -v
```

### Deposit reconciliation — three kinds of stuck

`reconcileDepositsOnce` (`services/transfers-svc/deposit_reconcile.go`)
extends the same worker that already reconciles transfers (`reconcile.go`,
see "Reconciliation: closing out pending transfers" above) — three
independent categories on every tick:

1. **`succeeded` staying `succeeded` too long instead of becoming
   `credited`** — not a separate category with its own polling, it's the
   same `creditSucceededDeposits` from the section above: it already tries
   to credit every `succeeded` deposit on every tick, so "stuck for a
   while" and "just became succeeded" are handled identically, with no
   special code for the "stuck" case.
2. **`pending` for too long with no webhook** (`reconcilePendingDepositsWithIntent`)
   — the deposit DOES have a `stripe_payment_intent_id` (meaning
   `createDeposit` successfully created the `PaymentIntent`), but the
   status hasn't moved in longer than `DEPOSIT_RECONCILE_STALE_AFTER`
   (default 2 minutes, same as transfers). The webhook might have been
   lost — polling Stripe directly (`PaymentIntent.Retrieve`) is standard
   practice: the webhook is the fast path, polling is the reliable
   fallback. `succeeded`/`canceled` resolve the status (through the
   `*IfPending` write variants — a race with the real webhook, if it does
   arrive at the same time, resolves the same way it does for transfers:
   the concurrent write simply finds no row). Any other Stripe status
   (`requires_action`, `processing`, ...) leaves the deposit as is — the
   payment is still in progress.
3. **`pending` with no `stripe_payment_intent_id`, older than N minutes**
   (`reconcileOrphanedPendingDeposits`) — garbage from the `POST /deposits`
   step: `createDeposit` failed (or the Stripe call itself failed) between
   the `INSERT` and writing `intent_id`. No money moved in either
   direction — it's simply marked `failed` (`abandoned_before_payment_intent`),
   with the extra guard `AND stripe_payment_intent_id IS NULL` — in case
   the original (slow) `createDeposit` call does still complete between
   the reconciliation's read and write.

### The most important invariant: reconciling `succeeded` vs `credited`

The whole reason the `succeeded` and `credited` statuses are kept
separate: there must never be a deposit where Stripe took the money but
the ledger never credited it, and that went unnoticed. `creditDeposit`
(`deposit_reconcile.go`) checks exactly this on every tick for every
`succeeded` deposit: if `now() - updated_at` exceeds
`DEPOSIT_RECONCILE_STALE_AFTER`, a separate, explicitly tagged log line is
written —
```
transfers-svc: DIVERGENCE ALERT: deposit <id> has been 'succeeded' ... for over 2m0s without becoming 'credited' ...
```
— regardless of the outcome of the crediting attempt on that same tick. In
a real bank this would be a paging alert; here it's an observable, easily
grep-able log line — exactly what a real bank calls reconciliation: not
just "fix the stuck one," but "prove nothing has diverged, and say so
loudly if it has."

### Reconciliation tests

`services/transfers-svc/deposit_reconcile_test.go`:
`TestCreditDeposit_LogsDivergenceAlertWhenStale` (intercepts
`log.SetOutput` and verifies the alert is actually written — not just
that the code for it exists),
`TestReconcilePendingDepositsWithIntent_ResolvesToSucceeded`/
`_ResolvesToFailedOnCanceled`/`_StillInProgressLeftPending`/
`_RespectsStaleness`, `TestReconcileOrphanedPendingDeposits_MarksFailed`/
`_RespectsStaleness`.
```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go test ./services/transfers-svc/... -run "TestReconcilePendingDepositsWithIntent|TestReconcileOrphaned|TestCreditDeposit" -v
```

### Refunds (`charge.refunded`): a reversal entry, not deleted history

The `ledger` is append-only: a deposit's original `entries` are never
deleted or edited. Compensating a refund is a **new** reversal entry
(`account_id` → genesis), not erasing history; that way both the original
credit and the fact that it was reversed stay visible.

What actually happens depends on how far the deposit got by the time
`charge.refunded` arrives (`processChargeRefundedEvent`,
`services/transfers-svc/webhook.go`):
- **`credited`** — the money is already in the user's ledger balance.
  Stripe took it back, so the books have to reflect that regardless of
  what the user's balance is right now (they may have already spent that
  money in other transfers) — `ledger.ReverseDeposit` posts with no
  balance check, the row moves to a `refunded` status.
- **`succeeded`** (Stripe confirmed, but crediting hadn't happened yet) —
  there's nothing to reverse, and this money can no longer be credited:
  the deposit is marked `failed` (`refunded_before_credit`).
- everything else (`pending`, already `failed`/`refunded`) — nothing to
  do, just a log line.

**A known MVP limitation**: if the user's balance has already been spent
(say, transferred to someone else) by the time the refund arrives,
`ReverseDeposit` still posts it, pushing the balance negative — real banks
show a negative balance/customer debt in this case, which requires full
handling (limits, collections, freezing) beyond this MVP's scope. It's
deliberately not solved here — only recorded as a fact.

**Why the ledger call happens before, not inside, the deduplication
transaction.** For every other event type, `processStripeEvent` wraps the
`INSERT INTO processed_stripe_events` and the `deposits` update in one
transaction (see above — that's how a retry after a failure never gets
lost in the "duplicate" branch). For `charge.refunded` that literally
doesn't work: the `ReverseDeposit` call is a network request to
ledger-svc, and holding a Postgres transaction open (with a row lock) for
the duration of a network call is bad practice (a slowdown or an outage in
ledger-svc turns into a held DB lock). So the order here is different:
`ReverseDeposit` (idempotent by `reference`, safe to repeat) runs
**before** any transaction, and deduplication + updating
`deposits.status` happens afterward, in a separate short transaction. The
`reference` for the reversal is **not** `deposit.ID` (that would collide
with the original credit's `reference` in ledger-svc's idempotency check),
it's `reversalReference(deposit.ID)` — a deterministic UUID derived from
`deposit.ID` via MD5 (a plain suffix string wouldn't work:
`entries.reference` is typed as `uuid`). A redelivery of the same
`charge.refunded` is safe on both counts: if the local write failed last
time, `ReverseDeposit` simply returns the same entry again; if it
succeeded, the status is no longer `credited`, and the retry doesn't even
try to reverse again (see the test
`TestStripeWebhookHandler_ChargeRefunded_CreditedDeposit`).

### Refund tests

`services/transfers-svc/webhook_test.go`:
`TestStripeWebhookHandler_ChargeRefunded_CreditedDeposit` (a reversal
through a fake ledger client, the reversal's `reference` ≠ `deposit.ID`, a
redelivery doesn't reverse again),
`TestStripeWebhookHandler_ChargeRefunded_SucceededNotYetCreditedDeposit`,
`TestStripeWebhookHandler_ChargeRefunded_PendingDepositIsIgnored`.
`services/ledger-svc/ledger_test.go`: `TestReverseDeposit_*` (see above).

### `POST /withdrawals` — withdrawing money, SIMULATION ONLY

**A real withdrawal to a card/account is not implemented and will not be
implemented in this project.** A payout to a real card/account (Stripe
Connect, ACH) requires a money transmitter license — a regulatory
requirement, not a technical one; no pet project can legally obtain one,
even running entirely in Stripe test mode.

What `createWithdrawal` (`services/transfers-svc/withdrawal.go`) actually
does: debits the user's internal balance through the ordinary, already
existing `ledger-svc.ExecuteTransfer` (`account_id` → genesis — the same
sufficient-funds check as any transfer, and the same mechanics, no new
code was needed in ledger-svc for this) and creates a row in a new
`withdrawals` table (`services/transfers-svc/migrations/000009`) with
status `payout_simulated`. Not a single call to the Stripe payout API ever
happens anywhere. Unlike `deposits`, `withdrawals` has no `pending`
status: the whole operation is synchronous (an ordinary gRPC call, not an
external API with async confirmation), so the row is written straight
away with its final status — `payout_simulated` or `failed`
(`insufficient_funds`).

**This must be made explicit on the frontend too**, once a screen for it
exists (a future step) — a user must never be given a reason to think the
money actually left for a card.

### `POST /withdrawals` tests

`services/transfers-svc/withdrawal_test.go`: `TestCreateWithdrawal_Success`
(verifies `ExecuteTransfer` was called `account_id` → genesis, status
`payout_simulated`), `TestCreateWithdrawal_InsufficientFunds` (status
`failed`, `ledger_transaction_id` unfilled — no money moved anywhere),
`TestCreateWithdrawal_InvalidAmount`,
`TestCreateWithdrawal_AccountNotActive`.
```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go test ./services/transfers-svc/... -run TestCreateWithdrawal -v
```

### Gateway: routing `/deposits` and `/webhooks/stripe`

`POST /deposits`, `GET /deposits/{id}`, and `POST /webhooks/stripe` are
new top-level prefixes on the Gateway (`gateway/main.go`, `newHandler`),
not nested under `/transfers`: `transfers-svc` itself registers them as
siblings of `/` on its own mux (`POST /deposits`,
`GET /deposits/{id}`, `POST /webhooks/stripe` — all with no shared
internal prefix), so proxying has to forward the path **unchanged**,
rather than stripping a prefix the way the rest of the routes do
(`gateway/proxy.go`'s `newProxy`/`http.StripPrefix`) — otherwise, say,
`POST /deposits` would turn into an empty path after stripping and land
on the transfer handler instead of the deposit one.

A trailing-slash nuance (the same nature as the comment in
`frontend/src/features/transfers/api.ts` about `POST /transfers/`): Go's
`http.ServeMux`, when registering `"/deposits/"` (with a slash — a subtree
pattern, needed to also catch `/deposits/{id}`), automatically
301-redirects a bare `/deposits` (no slash) to `/deposits/` — and a 301 on
a POST is dangerous: `fetch()` retrying the request after a redirect turns
it into a GET, silently dropping the body. The fix is registering
`/deposits` **twice**: an exact pattern (no slash, catches only literally
`/deposits`, no redirect) and a subtree pattern (`/deposits/`, catches
`/deposits/{id}`). `/webhooks/stripe` is registered with only the exact
pattern — there are no nested paths under it, and Stripe must never, under
any circumstance, run into a redirect on its own webhook.

`/webhooks/stripe` is also added to `publicPaths`
(`gateway/middleware.go`) — an exact string match, before any prefix
stripping. The JWT middleware never reads or touches the request body for
any path (headers only), so the raw bytes reach `stripeWebhookHandler`
unchanged automatically, with no special code needed for it — critical for
the signature check (see above).

**Tests**: `gateway/gateway_test.go` — brings up `newHandler` through
`httptest.Server` and a real HTTP round trip (not a mock): for
`POST /deposits` with no slash, checks there's no redirect and that the
backend received the path exactly as `/deposits` (unstripped) with the
body byte-for-byte; the same for `GET /deposits/{id}`; for
`/webhooks/stripe` — that the request goes through **without** a bearer
token, that the body arrives unchanged even with deliberately "ragged" JSON
formatting, and that a client-supplied `X-User-Id` still gets stripped
(even on a public path); plus a regression check that `/transfers/` still
gets stripped the way it always did.
```bash
cd gateway && go test ./... -v
```

### Gateway: routing `/profile`

The same trick as `/deposits` above, for the same reason: `GET /profile`,
`PATCH /profile`, `POST /profile/avatar/upload-url`,
`POST /profile/avatar/confirm` live on auth-svc's mux as siblings of `/`,
not under an internal `/profile` prefix — so the Gateway proxies them
**without stripping the path** (`newNoStripProxy`,
`gateway/proxy.go`), with the same double registration (`/profile` as an
exact pattern + `/profile/` as a subtree pattern), so `PATCH /profile`
with no slash never catches a 301 to `/profile/` — here that's even more
dangerous than with `/deposits`: a redirected `fetch()` demotes `PATCH`
to `GET`, silently turning a name update into a read that changes nothing.

`auth-svc` remains reachable on the old path too — `/auth/profile`
through the already-existing (stripping) `/auth` route. These are two ways
to reach the same handlers, not a migration from one to the other:
`/profile` is shorter and closer to how this entity should be thought of
from the frontend (a profile isn't about authentication), but nothing is
broken under the old path.

`/profile*` is **not** added to `publicPaths` — protected by default,
with `X-User-Id` injected from the JWT like everything else behind the
Gateway.

**Tests**: `gateway/gateway_test.go` — `TestNewHandler_ProfileExactPath_NoRedirectAndUnstripped`
(`PATCH /profile` with no slash: no redirect, path and body arrive
unchanged), `TestNewHandler_ProfileAvatarSubpath_NoRedirectAndUnstripped`,
`TestNewHandler_ProfileRequiresJWT`,
`TestNewHandler_ProfileStripsClientSuppliedUserID`, plus the regression
`TestNewHandler_AuthProfileStillStrips` — the old `/auth/profile` path
still gets stripped down to `/profile`, as before.

### Gateway: the `GET /ws` WebSocket endpoint

WS lives in the Gateway, not a separate service, for the same reason as
the routing above: the Gateway is the one place that already verifies the
JWT and knows the user's identity (`jwtMiddleware`,
`gateway/middleware.go`). Standing up a separate service for WS would mean
either duplicating the token check or routing it through yet another
internal call — and it's naturally the right place for stateful
connections to terminate anyway, the same place all the rest of the HTTP
terminates.

**Authentication happens on the first message, not a query parameter.**
The browser's WebSocket API gives no way to set `Authorization` on the
handshake. A token in `?token=...` would be simpler, but the whole URL
lands in proxy and server logs — the token would leak into logs. Instead
the connection is accepted unauthenticated and stays in that state until
the client sends `{"type":"auth","token":"..."}` as its first message
(`gateway/ws.go`, `wsServer.handleWS`). Until then the server sends
absolutely nothing into the socket. The wait is bounded by a timeout
(`WS_AUTH_TIMEOUT`, default 5s) — otherwise one client could hold open an
arbitrary number of unauthenticated connections with no token at all,
simply by never sending the auth message. That's why `/ws` is added to
`publicPaths` (`gateway/middleware.go`) — not because it needs no
authentication, but because it authenticates itself, a different way:
`jwtMiddleware` and `wsServer.handleWS` now share one `parseAccessToken` —
exactly the same signature/`user_id`/expiry check, just called from two
different places (a header versus the first message).

**The connection registry** (`wsRegistry`, `gateway/ws.go`) is
`map[user_id] -> a set of *websocket.Conn`, under a single `sync.Mutex`
(not `sync.Map`: the limit check and the insert have to be one atomic
operation, which `sync.Map` doesn't give without extra synchronization on
top). One user can have several tabs/devices; an insert is rejected once
a user already has `WS_MAX_CONNS_PER_USER` connections (default 5) —
otherwise one client could open thousands of them and exhaust the
process's memory. Registration and deregistration are logged
(`"gateway: ws connection registered/removed user=..."`) — that's how a
connection appearing/disappearing from the registry is verified. The
registry's shape is deliberately built for the Kafka consumer from a
later step to read from it too, to fan an event out to a specific user's
sockets — that fan-out itself doesn't exist yet at this step, the channel
is still empty.

**Heartbeat.** Once every `WS_HEARTBEAT_INTERVAL` (default 30s) the
server sends a ping and waits for a pong no longer than `WS_PING_TIMEOUT`
(default 10s) (`wsServer.runHeartbeat`). Without this, "dead" connections
— a client that disconnected with no close frame, a common case on
network loss — would pile up in the registry and leak memory forever. A
connection that doesn't answer a ping gets closed and deregistered.

**Token expiry on a live connection is a deliberate decision, not an
oversight: the token is only checked at the handshake.** An access token
lives 15 minutes, while a WS connection can hang around for hours, so
almost any connection will outlive its own token's expiry. That's
acceptable specifically because no data travels over WS — only signals
like "go refresh," and the client always fetches the actual data over an
ordinary HTTP request, where `jwtMiddleware` re-checks the token on every
call. Requiring re-authentication on a timer inside an already-open
connection would be extra complexity with no real benefit: what can be
compromised is whatever travels over the channel, and nothing more
sensitive than a "go refresh" notification ever does.

**Graceful shutdown.** Before this step, the Gateway had no SIGTERM
handling at all — the process was simply killed. Now `main()` listens for
SIGINT/SIGTERM via `signal.NotifyContext` and calls `http.Server.Shutdown`
with a `shutdownTimeout` (10s), like `notifications-svc`. But `Shutdown`
doesn't track hijacked connections — and a WebSocket upgrade is exactly a
TCP-socket hijack — so that timeout won't close them. Instead, the same
`context.Context` from `signal.NotifyContext` is passed straight into
`wsServer` (`newHandler(ctx, jwtSecret)`), and every `handleWS` handler
listens on it too, in parallel: the moment the context is done, the
connection gets a real close frame (`websocket.StatusServiceRestart`),
rather than just getting torn down as a dropped TCP connection — the
client sees the close immediately and starts reconnecting, instead of
waiting out a timeout on its own side. `main()` doesn't need to wait for
every WS goroutine to finish: sending a close frame isn't work that can be
"left unfinished," unlike `notifications-svc`'s Kafka consumers.

**Library:** `github.com/coder/websocket` (an actively maintained fork of
`nhooyr.io/websocket`), chosen over `gorilla/websocket` for its
context-native API (`Read`/`Ping`/`Close` take a `context.Context`, which
maps naturally onto the graceful-shutdown pattern above) and
`Conn.CloseRead(ctx)` — a ready-made way to run a push-only connection
(server → client) with automatic ping/pong/close handling, with no manual
`SetPingHandler`/`SetPongHandler`/read deadline.

**Tests**: `gateway/ws_test.go` — brings up `newHandler` through
`httptest.Server`, connects with a real WS client
(`coder/websocket`'s `Dial`): with no auth message the connection closes
after the timeout; with an invalid token, it closes immediately; with a
valid one, `{"type":"connected"}` arrives, the connection survives
several (shortened for the test) heartbeat intervals and answers pings; a
6th connection from the same user (the limit is shortened to 2 in the
test) is rejected.
```bash
cd gateway && go test ./... -v
```

**Manual verification** (`websocat`, `JWT_SECRET` and a token — as in the
other manual checks above):
```bash
# no auth message — closes after ~5s
websocat ws://localhost:8080/ws

# invalid token — closes immediately
websocat ws://localhost:8080/ws
{"type":"auth","token":"garbage"}

# valid token (from POST /auth/login) — stays open,
# answers {"type":"connected"}, outlives the heartbeat interval
websocat ws://localhost:8080/ws
{"type":"auth","token":"<access_token>"}

# a 6th connection with the same token — rejected
```
The matching pairs of lines are visible in the Gateway's logs at this point
`ws connection registered user=...` / `ws connection removed user=...`.

### Gateway: Kafka → WebSocket (`transfer.events` → signals to the browser)

Up to this point, the WS connection registry from the previous section was never
populated by anything — `transfer.events` (`TransferCompleted`/`TransferFailed`/`TransferRejected`,
`DepositCredited`) never reached it. This section wires the Gateway up as a consumer of
that same topic and turns events into WS pushes.

#### The multi-instance Gateway problem — and why each one gets its own consumer group

The Gateway scales horizontally: there can be N instances behind a load balancer, and a
given user's WS connection lives on exactly one of them. A Kafka event can land on an
instance that doesn't have that user at all — and the push would never arrive unless
something is done about it.

The fix: **every instance gets its own, unique consumer group**
(`newInstanceConsumerGroup`, `gateway/kafka.go` — `gateway-<hostname>-<random bytes>`,
generated fresh on every process start, never reused). With a shared group, Kafka would
split `transfer.events`'s partitions across instances, and each would only ever see part
of the events — exactly what can't happen here. With a unique group per instance, every
one of them gets **all** the events and decides **locally** whether to send anything: if
the user named in the event isn't currently connected to this particular instance,
`wsRegistry.send` is a silent no-op — not an error, not a log line (otherwise every
instance would log a line for every event belonging to someone else).

**The cost**: every instance processes the entire `transfer.events` stream, not its own
share of it. At pet-project scale that costs nothing. At real scale (dozens of
instances, high event volume), this is the point where, instead of N full Kafka
consumers, teams reach for Redis pub/sub or a separate realtime layer (say, every
Gateway instance subscribed to a Redis channel `user:<id>`, with a single Kafka consumer
process publishing into it) — the classic interview question about trading simplicity
for cost at scale.

`auto.offset.reset = latest` (`kafka.LastOffset`, not `FirstOffset`) is the other half
of the fix. A notification missed during a restart doesn't need catching up on: there
was no live WS connection to deliver it to anyway, and on reconnect the frontend
re-fetches current state over ordinary HTTP (a later step). The same logic as
`newNotificationReader` in notifications-svc (`services/notifications-svc/kafka.go:114-130`)
— and for the same reason: an email and a WS push are both live notifications, not a
projection that needs rebuilding from the start.

The one thing that does need to see the **entire** topic from the start is the
`account_id → user_id` cache (below): it's read by a fresh `FirstOffset` reader on
`account.events` (a compacted topic), because that's local state a freshly started
instance doesn't have at all yet.

#### `account_id → user_id`: a local TTL cache

Events carry `sender_account_id`/`recipient_account_id` (transfers) or `account_id`
(deposits) — the connection registry is keyed by `user_id`. The bridge between them is
`accountCache` (`gateway/accountcache.go`): `map[account_id]{user_id, expiresAt}` under
a mutex. Filled two ways:
- passively — a consumer of `account.events` reads `AccountCreated`, which deliberately
  carries `account_id` and `user_id` together (see
  `proto/events/v1/account_events.proto`, the same trick the `user_contacts` projection
  in notifications-svc already uses);
- on a miss — a direct synchronous call to `GET http://accounts-svc:8082/{id}`
  (`services/accounts-svc/http.go`, an already-existing, completely unauthenticated
  endpoint — like literally every internal call between services in this repo; no mTLS,
  no shared secret, no interceptor exists anywhere; this reuses an existing gap, not a
  new one).

The TTL (default one hour, `ACCOUNT_CACHE_TTL`) isn't about staleness: account
ownership never changes. It only bounds memory on a long-lived process; an evicted entry
just costs one extra HTTP request the next time that `account_id` shows up in an event.

#### Message format: a signal, not data

`{"type":"balance.changed"}`, `{"type":"transfer.updated","transfer_id":"..."}`,
`{"type":"deposit.updated","deposit_id":"..."}` — and nothing more. No amount, no new
balance. Reasons: delivery order isn't guaranteed (an event can arrive after a later one,
or not arrive at all, if the instance was holding the connection at send time but no
longer is by the time the event fires), meaning the client can't trust a value sitting
directly in the message — it could go stale silently. A signal instead of data forces
the client to re-fetch the authoritative value over ordinary HTTP, where both
authorization and consistency already exist.

Breakdown by event (`signalsForTransferEvent`/`signalsForDepositEvent`,
`gateway/notify.go` — pure functions with no Kafka/WS/HTTP inside, deliberately for the
next point's testability):

| Event | To whom | What |
|---|---|---|
| `TransferCompleted` | sender and recipient, each their own | `balance.changed` + `transfer.updated{transfer_id}` (money genuinely moved for both) |
| `TransferFailed` / `TransferRejected` | sender only | only `transfer.updated{transfer_id}` (the balance never changed) |
| `DepositCredited` | the account owner | `balance.changed` + `deposit.updated{deposit_id}` |

#### Security: a failed transfer's recipient is never resolved at all

The sprint 7 rule — "a recipient never sees someone else's failed transfers"
(`README.md`, notifications-svc — already described above) — is honored here not by
after-the-fact filtering, but structurally: `signalsForTransferEvent` calls
`resolve(recipientAccountID)` **only if** `eventType == TransferCompleted`. For
`TransferFailed`/`TransferRejected`, `recipient_account_id` is never even passed into
`resolve` — it's not just that no signal goes out, the very attempt to find out who that
is never happens. There's no "broadcast to everyone" anywhere in the code: `wsSignal`
always names exactly one recipient, `wsRegistry.send` always sends only to that one
`user_id`'s connections.

**Tests**: `gateway/notify_test.go` — `TestSignalsForTransferEvent_Completed_BothPartiesGetOwnSignalsOnly`
(both get only their own, each message's JSON checked for no extra fields),
`TestSignalsForTransferEvent_Failed_OnlySenderNotified_RecipientNeverResolved` and
`..._Rejected_...` (a spy resolver proves `recipient_account_id` was never even
requested — a direct test of the DoD case "a transfer blocked by fraud → a signal to A
only, nothing to B"). `gateway/ws_test.go`'s
`TestWSRegistry_Send_DeliversOnlyToTargetUsersLocalConnections` — the same guarantee at
the delivery level: two of user A's connections get the push, user B's connection gets
nothing even after waiting.
```bash
cd gateway && go test ./... -v
```

#### Manual verification

Through a full `docker compose up`: two `websocat` sessions with real tokens (from
`POST /auth/login`) for users A and B, a transfer A → B via `POST /transfers` — both get
their own `balance.changed`/`transfer.updated`, neither sees the other's data. A
transfer blocked by the fraud check — a signal to A only. The Gateway's log on startup
shows a line with its generated consumer group (`gateway: kafka consumer group
gateway-<hostname>-<...>`) — every running instance has its own.

### Tracing: an end-to-end trace_id across every service (OpenTelemetry + Jaeger)
A transfer request goes through Gateway → transfers-svc → fraud-svc → ledger-svc. Before this sprint, correlating these services' logs was only possible manually, by timestamp — and only if you got lucky and there were no concurrent requests. Now one transfer is one trace with a shared trace_id, showing every hop, every SQL query, and how long each one took.

- **`jaeger`** — all-in-one (collector + storage + UI in a single container). UI: **http://localhost:16686**.
- **`pkg/tracing`** — the one place OpenTelemetry gets configured. Every service calls `tracing.Init(ctx, "<its own name>")` once in `main()`.

### No service depends on Jaeger
`jaeger` has zero `depends_on` entries from any service, and a `tracing.Init` error is logged but **not fatal**. A bank that refuses to start because its observability system is unavailable has traded a real outage for an imaginary one. If Jaeger isn't up, the global provider stays a no-op from the OTel API, every `otelhttp`/`otelgrpc`/`otelpgx` hook turns into a cheap, non-recording span, and everything works exactly as before, just without traces. Tests get the same thing (they wire up handlers directly, bypassing `main()`), and so does `OTEL_SDK_DISABLED=true`.

### Auto-instrumentation
| What | Via | Wired up where |
|---|---|---|
| HTTP server | `otelhttp` | `tracing.Handler(mux, "<svc>")` in every `main()` |
| HTTP client | `otelhttp` | `tracing.Transport(nil)` — the Gateway's reverse proxy and `accountCache` |
| gRPC server | `otelgrpc` | `grpc.NewServer(tracing.GRPCServerOption())` |
| gRPC client | `otelgrpc` | `grpc.NewClient(..., tracing.GRPCClientOption())` |
| Postgres | `otelpgx` | `pkg/pgha.NewPool` — one place for all seven pools |

Postgres is wired up specifically in `pkg/pgha`, not in seven separate `main()`s: instrumentation that has to be remembered in seven places is instrumentation that's missing from one of them.

### Why the Gateway is the one place a trace can break
The typical mistake is framed as "a custom reverse proxy doesn't copy all the headers, and the trace breaks at the first hop." Here it's a little more subtle, and the distinction matters: **the Gateway is the root of the trace**. A request from the browser carries no `traceparent` at all, so there's nothing to copy — `httputil.ReverseProxy` will faithfully copy every incoming header and still forward nothing. The header has to be **created** from the server span the Gateway just opened, and that's what `Transport` does:

```go
func tracedReverseProxy(target *url.URL) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = tracing.Transport(nil)  // ← this line right here
	return proxy
}
```

Without it, the breakage is silent and has a very specific signature: Jaeger shows a trace made of a single Gateway span, a **separate** root trace for transfers-svc, and another one for fraud-svc — three disconnected traces per request, each of which looks fine on its own.

### What does NOT go into spans
A trace is as much a leak channel as a log, and Jaeger in this compose stack is protected by nothing at all: no authentication, no TLS, no retention policy. The rule and its reasoning live in `pkg/tracing/attributes.go`, next to the attribute keys themselves, not in a separate document that would drift from the code. In short:

- **no credentials of any kind**: JWTs, access/refresh tokens, session identifiers, passwords and their hashes, the JWT signing secret;
- **no emails**, names, or anything that identifies a person;
- **no card data** and **no Stripe `client_secret`** — it authorizes confirming a payment, meaning it is a credential, regardless of the fact that it sits in a response body;
- **no account numbers** (`account_number`) — that's the thing a user reads out loud to someone. Internal account UUIDs can and should be included: they're pseudonymous, meaningless outside this database, and without them a trace can't be tied back to the row it touched.

The rule is stricter than "don't log secrets," because auto-instrumentation records data without asking: `otelhttp` logs the URL, and a secret in a query parameter would land on a span even though not one line of this repo ever put it there. An audit was done — there are no secrets in URLs anywhere in this codebase: verification codes, passwords, and tokens all travel in a JSON body, the WS token arrives as a separate message after connecting, and the only query parameters (`limit`, `cursor` on `GET /transfers`) carry nothing sensitive. For the same reason, `otelpgx` **deliberately does not enable** `WithIncludeQueryParameters`: it attaches every bind argument to the span, and among them are password hashes (`users`), emails (the `user_contacts` projection), and tokens. One boolean flag would ship all of that into an unprotected Jaeger, and no single line of code would look guilty for it.

### Amounts in spans — a deliberate decision
`neobank.amount_minor` is put on the span. This is a decision, not an oversight: an amount is commercially sensitive, and paired with a timestamp it's a weak identifier. It's included anyway, because a trace of money moving with no amount on it fails to answer the questions tracing exists for in the first place: "was the transfer rejected for its amount, or for a velocity rule?", "did the ledger record what transfers-svc meant to record?" It's stored as a bare integer, with no currency and no account number next to it — a leaked span says "600000," not "€6000 from account X to account Y."

### Errors, and what is NOT an error
`tracing.Fail(ctx, "<type>", err)` sets the span's status to error and attaches a low-cardinality `neobank.error.type` — that's what lets Jaeger filter "show me every failed transfer." That's the whole point of tracing, so it matters exactly what falls into this category:

- **An error**: `fraud_check_unavailable` (fraud-svc unreachable, the transfer stuck in pending), `settlement_uncertain` (unknown whether the payment went through in the ledger — the exact case the reconciliation worker later cleans up), `ledger_execute_failed`, `create_transfer_failed`.
- **NOT an error**: a fraud rejection, and `insufficient_funds` from the ledger. The system did exactly what it was supposed to do. Marking these as errors would dump every blocked transfer into Jaeger in the same bucket as real failures — and that's exactly the distinction someone hunting for problems needs. The story is told by the `neobank.fraud.decision` and `neobank.ledger.outcome` attributes instead, with nothing declared broken.

### Sampling
Locally — 100% (`AlwaysSample`). **That's not viable in production**: storage cost and volume scale linearly with traffic. The usual answer is `ParentBased(TraceIDRatioBased(x))` plus a tail-based sampler that keeps every trace containing an error regardless of the percentage.

`ParentBased` is already in place here, and it's not decoration: the sampling decision is made **once, at the root** (the Gateway) and travels downstream in the `sampled` flag of the `traceparent` header. Without `ParentBased`, every service would roll its own dice, and at any percentage below 100 the result would be half-sampled traces — meaning useless ones.

### Manual verification
```
docker compose up -d

# make a transfer through the UI (or curl, see "Moving money through the ledger"),
# then open http://localhost:16686 and pick service=gateway

# what should be in the service list — all six, under their real names
curl -s http://localhost:16686/api/services
# {"data":["jaeger-all-in-one","auth-svc","accounts-svc","gateway",
#          "ledger-svc","fraud-svc","transfers-svc"], ...}

# find the trace by transfer_id (the tag lives on the transfers-svc span, not the Gateway's)
curl -s 'http://localhost:16686/api/traces?service=transfers-svc&lookback=1h&limit=5&tags=%7B%22neobank.transfer.id%22%3A%22<TRANSFER_ID>%22%7D'
```

A successful transfer — one trace, 55 spans, correct nesting (abridged):

```
gateway            POST /transfers/                              118.39ms
  gateway            POST /                                      118.31ms   ← the proxy's client span
    transfers-svc      POST /                                    116.63ms
      transfers-svc      AccountsService/GetAccountByUserID         2.18ms
        accounts-svc       AccountsService/GetAccountByUserID       0.79ms
          accounts-svc       query SELECT                           0.52ms
      transfers-svc      query INSERT                               8.50ms
      transfers-svc      FraudService/CheckTransfer                15.54ms
        fraud-svc          FraudService/CheckTransfer              13.96ms
          fraud-svc          query SELECT / INSERT / COMMIT         ...
      transfers-svc      LedgerService/ExecuteTransfer             13.61ms
        ledger-svc         LedgerService/ExecuteTransfer           12.32ms
          ledger-svc         query BEGIN                            0.43ms
          ledger-svc         query INSERT                           0.59ms   ← one of the two entries
          ledger-svc         query INSERT                           0.56ms   ← the other half of the pair
          ledger-svc         query COMMIT                           8.19ms
      transfers-svc      query UPDATE / INSERT / COMMIT             ...
```

A transfer blocked by fraud — **the branch cuts off at fraud, ledger is never reached** (32 spans, `ledger-svc` is entirely absent from the trace):

```
      transfers-svc      FraudService/CheckTransfer                 8.98ms
        fraud-svc          FraudService/CheckTransfer               8.52ms
      transfers-svc      query UPDATE      ← status = 'rejected'
      transfers-svc      query COMMIT
```

with attributes:

```
fraud-svc:neobank.fraud.decision       = reject
fraud-svc:neobank.fraud.triggered_rule = amount_threshold
transfers-svc:neobank.transfer.status  = rejected
transfers-svc:neobank.amount_minor     = 600000
```

### Operation-name cardinality
Span names become the operation list in Jaeger, so `/deposits/<uuid>` collapses to `POST /deposits/{id}` (`tracing.SpanName`). Without this, every request would add a new, permanent entry to the dropdown. `otelhttp` can't do this on its own: it wraps the mux from the outside, meaning routing hasn't happened yet at the moment the span is created, and the route template is unknown.

Excluded from traces: `/healthz` (a docker probe every 5 seconds per service — these would make up the overwhelming majority of spans and crowd out real traces) and `/ws` (a span lives as long as the request, and a WebSocket request lives for hours — that's not a useful trace, it's one giant span that never closes).

### What broke along the way
`resource.Merge` refuses to merge resources with different schema URLs and **returns an error** instead of picking one. `pkg/tracing` was built against `semconv/v1.26.0`, while `resource.Default()` in SDK 1.45 uses `v1.43.0`. The result: all seven services logged one line, `tracing disabled: conflicting Schema URL`, and kept running completely normally, emitting zero spans; Jaeger knew about exactly one service — itself. From the outside this looked like a fully healthy stack, and the only symptom was an absence. The regression test is `TestInit_SucceedsWithoutACollector` in `pkg/tracing`: it checks exactly what broke (that `Init` doesn't return an error), and it needs no collector for that, because the OTLP exporter connects lazily.

### The async side: carrying context through the database
The synchronous chain is the easy part. The system is asynchronous: `transfers-svc` writes an event to the outbox, the relay publishes it to Kafka seconds later, `notifications-svc` consumes it and sends an email. The trace used to break at the outbox write — exactly where the most interesting debugging starts.

**Auto-instrumentation doesn't solve this, and it matters to understand why.** Any standard Kafka wrapper propagates context by injecting it into headers *at publish time*, from a span alive on the publishing goroutine. No such span exists here: a table sits between the original operation and the publish. The transaction has committed, the HTTP span has closed, the response has already gone to the browser — and only afterward does a separate relay goroutine, which never heard of that request, read the row and publish it. There's nothing to inject, and nowhere to inject it from.

The only thing that crosses that gap is what carries the context across it: the row in the database itself.

```
                 HTTP span closed        │        the relay wakes up
 request ──► transaction ──► COMMIT ─────┼──────────► SELECT ──► Kafka ──► consumer
              │                          │              │
              └─ trace_context ──────────┼──────────────┘
                 (same transaction)      │   (restored, as a span link)
```

`ALTER TABLE <outbox> ADD COLUMN trace_context JSONB` on all three outbox tables. `outbox.InsertEvent` serializes the current context (`propagator.Inject` into a map carrier → JSON) **in the same transaction** as the event itself: the context is exactly as durable as the event, and an event can't exist with no record of its own origin. JSONB, not TEXT, so this reads straight out of psql with not one line of code:

```sql
SELECT event_type, trace_context->>'traceparent' FROM outbox WHERE published_at IS NULL;
```

The column is nullable, and that's part of the contract: rows written before it existed, and rows written while tracing was disabled, legitimately have no context. No trace is no trace, not an error.

### Linking spans: a link, not a parent
The most substantive decision of this sprint, so it gets a full explanation.

The parent span (the transfer's HTTP request) finished long ago by the time the relay publishes the event. Making the publish its child span is technically possible — the context sits right there in the row, `Extract` returns a valid `SpanContext`, and passing it to `Start` is all it would take. **Don't do this.**

A parent span's duration, by definition, *contains* its children's durations. The request took 50ms; the publish happens a second later, and after a Kafka outage, minutes later. The result is Jaeger drawing exactly what it was told: a 50ms-long bar, with a child span starting three seconds after it ended. That's not cosmetic — it breaks everything computed off the parent: request p99, "how long did processing take," any duration aggregate. And the reader of the trace has to remember that nesting here doesn't mean what it usually means.

A **span link** says exactly what's actually true: this is separate work, causally related to that trace. Implemented as `tracing.StartLinkedRoot`:

- `trace.WithNewRoot()` — a new trace, even if the relay's goroutine happens to have a span of its own alive (without this, the entire relay would degenerate into one endless trace with every published event hanging off it — checked separately by a test);
- `trace.WithLinks(link)` — a link back to the originating trace.

Jaeger shows both, and the jump works in both directions.

**What goes into the Kafka headers is the context of the DELIVERY, not the original request.** That means consumers become ordinary child spans of the publish, in the same trace as it. That's correct: they really are triggered by that publish and follow it closely in time, so the nesting is honest here. So one transfer ends up as two traces:

```
trace A: gateway → transfers → fraud → ledger          (the synchronous request)
             ▲
             │ link
trace B: relay publish → notifications-svc → email     (event delivery)
                       → gateway → WebSocket push
```

### Consumers
`notifications-svc`, `gateway` (the WS push), and `accounts-svc` pull the context out of the headers (`traceContextFromMessage` → `tracing.ExtractMap`) at the start of message handling and open a span from it. The helper is duplicated across three services rather than pulled into a shared package — for the same reason the `event_type` constant is duplicated: what's shared here is the wire format, not the code.

Retries and the DLQ get their own spans with an attempt number:

```
notifications handle transfer.events            (the message's whole path, retries included)
  attempt          neobank.retry.attempt = 1
  attempt          neobank.retry.attempt = 2
  attempt          neobank.retry.attempt = 3
  dlq publish      neobank.dlq.sent = true      [ERROR]
```

Landing in the DLQ **is** a delivery failure, so that span is marked as an error (unlike a fraud rejection, which isn't one: there the system did exactly what it should).

### Reconciliation in the trace
Reconciliation workers wake up on a timer — their spans have no parent by construction, nothing ever called them. So `transfers` and `deposits` also got a `trace_context` column, filled in when the row is created, with the worker linking to it when it resolves the row.

This produces the single clearest demonstration in the whole project: **the entire story of a stuck transfer becomes visible** — the original request, the exact point it stalled short of a terminal state, and, minutes later, a separate trace of the worker that finally finished the job.

`RECONCILE_STALE_AFTER` is exposed via `.env` (empty by default ⇒ 2 minutes from the code) purely so this could be demonstrated without waiting two minutes for every attempt.

### Manual verification
```
# 1. An ordinary transfer. In Jaeger, find the request's trace, then on the
#    second trace's "outbox publish TransferCompleted" span — the link back to the first.

# 2. A stuck transfer: with fraud-svc down, a transfer stays pending
docker compose stop fraud-svc
curl -s -X POST http://localhost:8080/transfers/ -H "Authorization: Bearer $TOKEN" \
  -H "Idempotency-Key: stuck-1" -H 'Content-Type: application/json' \
  -d '{"recipient_iban":"IE34ZZZZ00004234567890","amount":2500}'
# {"status":"pending","message":"fraud check unavailable, transfer still pending"}
docker compose start fraud-svc
# wait out RECONCILE_STALE_AFTER + one worker tick (30s)
```

The delivery trace (verified, 17 spans):

```
transfers-svc      outbox publish TransferCompleted     1007.27ms  --link--> the request's trace
  transfers-svc      query UPDATE                          1.06ms            (published_at)
  notifications-svc  notifications handle transfer.events  35.87ms
    notifications-svc  attempt                             34.74ms
      notifications-svc  query INSERT / SELECT / UPDATE      ...             (claimEvent, the email)
  gateway            gateway handle transfer.events         1.28ms           (the WebSocket push)
```

The 1007ms on the root span isn't work, it's waiting: the relay polls the outbox once a second. That exact delay is precisely what would make a parent link wrong.

The reconciliation trace (verified):

```
reconcile transfer   --link--> the original request's trace (25.4s earlier)
  neobank.reconcile.trigger            = stale_pending
  neobank.reconcile.stale_for_seconds  = 25.37
  neobank.reconcile.resolution         = failed
  neobank.transfer.status              = failed
```

### What's left out of scope
Context doesn't propagate through `auth_outbox`→`user.events`→`accounts-svc`→`ledger-svc` for registration (the mechanism is the same and works, but that chain isn't part of this step's DoD), and `withdrawals` has no `trace_context` column — it has no reconciliation worker that would ever read one.

## Frontend: the deposit screen (`/deposit`)

`frontend/src/features/deposits/` — an amount form → `POST /deposits` →
Stripe Elements (`PaymentElement`) → `stripe.confirmPayment` with
`redirect: 'if_required'` (doesn't navigate away for most cards,
including most 3D Secure scenarios — Stripe.js handles the challenge
inline). The publishable key (`pk_test_...`) is `VITE_STRIPE_PUBLISHABLE_KEY`
(`frontend/.env`, see `.env.example`); `loadStripe(...)` is called once
at module scope (`features/deposits/stripe.ts`), not on every render.

**Honesty about the moment of crediting** is the most important part of
this screen. A successful client-side `confirmPayment` only means "Stripe
accepted the payment" (`deposits.status = 'succeeded'`) — the balance
hasn't changed yet; that's a separate fact that arrives later, when the
background worker posts the ledger entry (`status = 'credited'`, see
"Crediting: succeeded → credited" above). So the screen **does not show**
"balance topped up" right after `confirmPayment` — only "payment
accepted, crediting within a minute," and then polls `GET /deposits/{id}`
(`features/deposits/useDepositStatusPolling.ts`) every 2 seconds until
the status becomes `credited` (at which point `invalidateQueries` runs on
`['accounts','me']` and `['transfers','history']` — the same keys
`TransferForm` already invalidates after a transfer — and the dashboard
balance updates itself, no F5 needed) or `failed`/`refunded`. If 60
seconds pass (twice `reconcileInterval` — see `reconcile.go` — leaving
room for one missed worker tick) with crediting never happening, the
spinner stops and shows "crediting is being processed, check your balance
later" instead of loading forever.

A declined card (an `error` from `confirmPayment`) sends the user back to
the amount-entry step with a clear message — a new attempt always means a
new `POST /deposits` (a new `PaymentIntent`, a new `client_secret`): the
old `client_secret` is never reused, exactly as required.

### Operation history: a unified feed of transfers, deposits, and withdrawals

`GET /transfers` (the name stayed the same, even though it now returns
more than transfers — see `services/transfers-svc/history.go`'s
`historyEntry`) now returns a merged, genuinely cursor-paginated feed of
operations: transfers, deposits, and (simulated) withdrawals, sorted by
time, with a `type` field to tell them apart. Implemented not as one
heterogeneous SQL UNION (which would have meant padding out incompatible
columns across three different tables with NULLs), but as a merge at the
Go level: each source (`getTransferHistoryPage` — the existing 2-way
UNION over sender/recipient; the new
`getDepositHistoryPage`/`getWithdrawalHistoryPage` — a single
`account_id` column each) is queried independently against the same
`(created_at, id)` cursor, and the results are merged, re-sorted, and cut
down to `limit`. This is exact, not approximate — by the same reasoning
as the existing 2-way UNION for transfers (see the comment on
`transferHistoryQueryFirstPage`): the top-`limit` of a merge of N
independently sorted sources is always contained within each source's own
top-`limit`.

`hasMore` requires an OR, not just "did the merged pool exceed `limit`":
if one source on its own already returned `hasMore=true` (it has more
rows past its own top-`limit` that never even made it into the merged
pool), the final page has to signal `hasMore=true`, even if the merged
pool after merging came out to exactly `limit` rows, not more. (See the
test `TestGetOperationHistoryPage_HasMoreReflectsEachBranch`.)

On the frontend — `features/transfers/components/OperationHistory.tsx`
(formerly `TransferHistory.tsx`): every row got a type badge ("Transfer"
/ "Deposit" / "Withdrawal") and an explicit "· simulated" tag for
withdrawals (`payout_simulated` is currently unreachable from the UI — a
withdrawal has no creation form, only a display, should a row ever show
up through the API directly). This also fixed a constant that had
drifted from the backend, `direction === 'sent'` (the backend has always
returned `'outgoing'`/`'incoming'` — see `services/transfers-svc/http.go`)
— the comparison previously never matched on any real response.
```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go test ./services/transfers-svc/... -run "TestGetDepositHandler|TestGetDepositHistoryPage|TestGetWithdrawalHistoryPage|TestGetOperationHistoryPage" -v
```

### Stripe test cards

Verified locally (Stripe test mode, via `stripe listen` — see above):

- `4242 4242 4242 4242`, any future expiry, any CVC, any index —
  a successful payment (`payment_intent.succeeded`).
- `4000 0000 0000 0002` — a guaranteed decline
  (`payment_intent.payment_failed`, `card_declined`).

Not verified live, but documented as an option (the task explicitly says
testing 3D Secure is optional): `4000 0025 0000
3155` — requires completing a 3D Secure challenge; with
`redirect: 'if_required'`, Stripe.js handles it inline (a modal over the
page), with no navigation to `return_url`.

## fraud-svc: connecting to Postgres, the schema

`fraud-svc` got a Postgres connection and a schema (`pgx/v5` + `golang-migrate`, the same pattern as `ledger-svc`/`accounts-svc`/`transfers-svc`: migrations are embedded via `//go:embed migrations/*.sql` and applied automatically at startup, with a namespaced migrations-version table, `schema_migrations_fraud_svc`, since every service shares one physical `neobank` database). The check logic itself arrived in a later step (see "fraud-svc: rule-based transfer scoring" below), and its integration into the transfer flow in a step after that (see "fraud check before the ledger" at the very bottom).

Two tables:
- **`fraud_rules`** — rule configuration (`rule_type` — `amount_threshold` / `velocity_count` / `velocity_sum`, unique; `enabled`; `threshold_value` — an amount in cents or a count, depending on the rule type; `window_seconds` — the window for velocity rules, `NULL` for a single-amount threshold). A mutable table, edited by hand or by a future admin API.
- **`fraud_checks`** — an append-only log of every check (`transfer_id`, `account_id`, `amount`, `decision` — `approve`/`reject`, `triggered_rule`, `details` as JSONB with the computed values). The `idx_fraud_checks_account_id_created_at` index on `(account_id, created_at)` is built for exactly what the velocity rules need (counting transfers/summing amounts for an account over the last N seconds). The log exists for auditability: when a user asks "why was my transfer blocked," the answer has to be in the data, not a guess; the same table is future training data for ML scoring too (out of scope for this step).

Three default rules are seeded by migration `000003_seed_default_fraud_rules` (the values are an MVP starting point, changed through the `fraud_rules` table itself, with no new migration needed):
| `rule_type` | `threshold_value` | `window_seconds` | Meaning |
|---|---|---|---|
| `amount_threshold` | 500000 (€5,000 in cents) | `NULL` | an unusually large single transfer |
| `velocity_count` | 5 | 300 (5 min) | more than 5 transfers in 5 minutes — looks like a compromised session/script |
| `velocity_sum` | 1000000 (€10,000 in cents) | 3600 (1 hour) | draining an account via a series of transfers, each individually below the single-amount threshold |

`GET /healthz` now checks a real Postgres connection (`SELECT 1` with a 2s timeout, `503` on error) — like the other Postgres-backed services, instead of the DB-less handler from `pkg/health`.

## fraud-svc: rule-based transfer scoring (`CheckTransfer`)

`fraud-svc` now computes decisions, not just holds a schema. The gRPC contract is `proto/fraud/v1/fraud.proto` (`fraud.v1.FraudService.CheckTransfer(transfer_id, account_id, amount) → {decision, triggered_rule, reason}`), and the server runs on a separate port (`GRPC_PORT`, default `9085`) alongside the already-existing HTTP one (`8085`) — the same pattern as `accounts-svc` (HTTP and gRPC on different ports in one process, a plain `grpc.NewServer()` with no options, standard `grpc.health.v1.Health` + `reflection.Register`). `transfers-svc` now calls it — see "fraud check before the ledger (transfers-svc → fraud-svc)" at the end of this section.

### Principle: rule-based, not ML

Rules from `fraud_rules` are checked in a fixed order — `amount_threshold` → `velocity_count` → `velocity_sum` — and **the first rule that fires immediately yields `reject`**; nothing further is checked. This keeps the decision explainable: `triggered_rule` always names exactly one rule, never "some combination" of several. Disabled rules (`enabled = false`) are skipped entirely.

- **`amount_threshold`**: `amount > threshold_value` → reject. A single transfer above the threshold.
- **`velocity_count`**: the count of this `account_id`'s approved checks within `window_seconds`, **plus this very check** (i.e. "what if this one were approved too"), exceeds `threshold_value` → reject.
- **`velocity_sum`**: the same, but the sum of approved transfers over the window (plus this one's amount) instead of a count.

The data source for both velocity rules is fraud-svc's **own** `fraud_checks` table (`WHERE decision = 'approve'`), never `transfers-svc`'s database: every service owns its own data and computes off it — someone else's database is never the source for someone else's logic.

The observed value for every rule actually checked (including the one that fired) is put into `details` (JSONB) alongside the threshold and window — after the fact it's visible not just what was blocked, but exactly what was computed.

### Fail-closed, not fail-open

If `fraud-svc` can't compute a decision (a Postgres error at any step — reading rules, computing velocity, writing the log), `checkTransfer` returns an error, and the RPC returns the gRPC status `codes.Internal`, **not** a silent `approve`. Reading rules, computing velocity, and writing to `fraud_checks` are wrapped in one transaction: either the decision is fully computed and logged, or nothing happens at all — there's no such thing as a partial write with no final decision. What to do when fraud-svc is unavailable (let the transfer through, reject it, queue it) is the caller's (`transfers-svc`'s) decision, not fraud-svc's own.

### Every check is a row in `fraud_checks`

Both `approve` and `reject` are logged, not just rejections — that's what makes computing the velocity rules possible at all (they need the full history of approved checks), and it's the same thing auditability from the previous step needs.

### Manual verification
```bash
grpcurl -plaintext localhost:9085 list

grpcurl -plaintext -d '{"transfer_id": "<uuid>", "account_id": "<uuid>", "amount": 1000}' \
  localhost:9085 fraud.v1.FraudService/CheckTransfer
# {"decision": "approve", "reason": "no rule triggered"}

grpcurl -plaintext -d '{"transfer_id": "<uuid>", "account_id": "<uuid>", "amount": 600000}' \
  localhost:9085 fraud.v1.FraudService/CheckTransfer
# {"decision": "reject", "triggeredRule": "amount_threshold", "reason": "amount_threshold: observed 600000 exceeds threshold 500000"}
```
Manually verified (through the `fullstorydev/grpcurl` image on the `neo-bank_default` docker network, since `grpcurl` isn't installed locally): both calls above behaved as described, and `SELECT * FROM fraud_checks WHERE account_id = '<uuid>'` showed both rows — `approve` and `reject` — with the expected `decision`/`triggered_rule`.

### Tests

`services/fraud-svc/fraud_test.go` — unit tests against a real Postgres (repo convention: `DATABASE_URL`, the test skips itself if the variable isn't set), one scenario per rule: a transfer above the threshold → `reject`/`amount_threshold`; a 6th transfer inside the five-minute window → `reject`/`velocity_count`; a sum above the limit inside the one-hour window → `reject`/`velocity_sum`; an ordinary transfer → `approve`; a disabled rule is skipped; a canceled context (simulating a DB failure without actually disconnecting Postgres) → an error and **not a single** row written to `fraud_checks` (fail-closed, no partial write).
```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go test ./services/fraud-svc/... -v
```

## fraud check before the ledger (transfers-svc → fraud-svc)

`transfers-svc` now calls `fraud-svc.CheckTransfer` as part of creating a transfer, **after** the pending row has already been inserted, but **before** calling `ledger-svc.ExecuteTransfer`:

```
createTransfer() → pending row created
        ↓
checkTransferFraud() → approve   → settleTransfer() as before (completed/failed/uncertain)
                      → reject   → status='rejected', ledger is NEVER called
                      → uncertain (fraud-svc unavailable) → the row stays pending, ledger is NOT called
```

The order is deliberately this way, not "check fraud first, then create the row": a rejected transfer keeps a row with a reason (the user sees in their history what happened and why it was blocked), while the money is guaranteed never to have moved, because the ledger call is simply never reached. This is also what makes the reject path compensation-free — there's nothing to roll back, because nothing was ever touched; a well-designed saga saves on compensation by putting its riskiest step (the ledger) last.

### `rejected` — a new status, separate from `failed`

`failed` is a technical failure or insufficient funds (the `ledger-svc` level); `rejected` means blocked by the fraud check. These are different things both to the user and for analytics, so they aren't collapsed into one status: migration `000003_add_rejected_transfer_status` adds `'rejected'` to the CHECK on `transfers.status`. No separate column was added for the reason — `failure_reason` was reused: it already stored "why it wasn't completed" (`ledger-svc`'s codes), and now on `rejected` it holds fraud's `triggered_rule` (e.g. `"amount_threshold"`). The `status` column alone always tells you unambiguously which vocabulary to read it against — adding a `rejection_reason` for purely cosmetic separation would have meant touching every `RETURNING`/`SELECT` in the file for zero semantic benefit.

### Fail-closed vs fail-open — a deliberate choice

If `fraud-svc` is unreachable, or it returns an error itself (`codes.Internal` — the only code it ever returns; unlike `ledger-svc`, it has no other business codes), `transfers-svc` literally has two options:

- **fail-open** — let the transfer through with no check. More available (a transfer never gets stuck if fraud-svc goes down), but it's a hole: an attacker who knows fraud-svc can be taken down (or simply hits the window of a real outage) pushes a transfer through with no check at all.
- **fail-closed** — don't let it through. Safer, but the transfer doesn't complete until fraud-svc responds.

**Fail-closed** was chosen: the transfer stays `pending` (no row is written — the same logic as `ledger-svc`'s undetermined outcome), and the client gets `202` with `"message": "fraud check unavailable, transfer still pending"`. In this case, no money moved at all: `ledger-svc` was never even called, so there's nothing to roll back either. A real bank would choose exactly this way: "the fraud check is unavailable" is a state where money must stand still, not one that gets silently waved through for the sake of availability. The same reason `checkTransferFraud` doesn't do a code-by-code breakdown the way `settleTransfer` does (there `ledger-svc` encodes business outcomes through gRPC statuses; here `fraud-svc` has nothing like that — any error means "couldn't compute a decision," and that's exactly what should lead to fail-closed rather than guessing).

### Idempotency

The fraud check is called **strictly after** `createTransfer`'s outcome switch in `http.go` has already handled every early `return` (including `createTransferReplayed`) — i.e. at exactly the same point the call to `settleTransfer` already sits at today. A retry with the same `Idempotency-Key` returns the existing row's current state through the short-lived path and never even reaches the fraud call — the same mechanism that already keeps a retry from calling `ledger` twice. If the row is still `pending` because of an undetermined fraud-check outcome, a retry will keep returning that same `pending` snapshot, never trying to recheck fraud — but it won't stay stuck forever: the reconciliation worker (see "Reconciliation: closing out pending transfers" below) resolves this case too, checking `ledger-svc` directly, regardless of what caused the uncertainty.

### Manual verification
```bash
# a legitimate transfer — approve, the money moves
curl -s -X POST http://localhost:8080/transfers/ \
  -H "Authorization: Bearer <access_token>" -H "Content-Type: application/json" \
  -H "Idempotency-Key: <uuid>" \
  -d '{"recipient_iban":"IE34ZZZZ00004234567890","amount":1000}'
# {"status":"completed","ledger_transaction_id":"..."}

# a transfer above the threshold — reject, the ledger is never called
curl -s -X POST http://localhost:8080/transfers/ \
  -H "Authorization: Bearer <access_token>" -H "Content-Type: application/json" \
  -H "Idempotency-Key: <uuid>" \
  -d '{"recipient_iban":"IE34ZZZZ00004234567890","amount":600000}'
# {"status":"rejected","failure_reason":"amount_threshold"}

# fraud-svc unavailable
docker compose stop fraud-svc
curl -s -X POST http://localhost:8080/transfers/ \
  -H "Authorization: Bearer <access_token>" -H "Content-Type: application/json" \
  -H "Idempotency-Key: <uuid>" \
  -d '{"recipient_iban":"IE34ZZZZ00004234567890","amount":1000}'
# 202 {"status":"pending","message":"fraud check unavailable, transfer still pending"}
docker compose start fraud-svc
```
Manually verified on the full stack (two real users via `/auth/register` → `/auth/verify-email` → `/auth/login`, the sender funded through `cmd/devtopup`): a legitimate transfer — `completed`, with an `approve` row in `fraud_checks`; a 600000 transfer — `rejected`/`amount_threshold`, neither side's balance changed, `ledger_transaction_id` is empty; six quick small transfers in a row — the 6th is `rejected`/`velocity_count`; with `fraud-svc` stopped — `202 pending`, the balance is unchanged, `fraud_checks` for this transfer is empty; a retry of the same `Idempotency-Key` (both after a reject and after fraud-unavailability) — the same response, the row count in `fraud_checks` for this `transfer_id` doesn't grow.

### Tests

`services/transfers-svc/transfer_test.go` — `fakeFraudClient` (the same pattern as `fakeAccountsClient`/`fakeLedgerClient`: embeds the real interface as nil, overrides only `CheckTransfer`). `TestCheckTransferFraud_Approved`/`_Rejected`/`_Uncertain` (table-driven over `codes.Internal`/`codes.Unavailable`, proving fail-closed fires on **any** error, not just the documented one)/`_UnexpectedDecision` (an unfamiliar `decision` value — also fail-closed, not a silent approve). The HTTP handler isn't tested separately — the repo has no tests at all at the HTTP-handler/gRPC-server level (only at the business-logic level below it), and the guarantee "a retry never calls fraud twice" is structural here (an early `return` before the call site), not a separate test — the same as is already true for `settleTransfer`.
```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go test ./services/transfers-svc/... -v
```

## Reconciliation: closing out pending transfers (transfers-svc)

This is the real saga problem in the transfer flow — not a fraud reject (the ledger is never touched there, nothing to compensate), but exactly the broken-connection case after calling `ledger-svc.ExecuteTransfer`, flagged with a TODO back in "An honest boundary" above: `transfers-svc` called `ledger`, `ledger` posted the entry and committed it, and the response never arrived (a timeout, a network blip, or `transfers-svc` itself crashing between the call and writing `completed`). The row hangs in `pending` forever — the money genuinely moved, and the system has no idea.

### Why this isn't "roll it back"

Compensation in a saga isn't a DB rollback. An `entries` row is append-only and never physically deleted. There are only two ways to compensate: **confirm** it (if the entry really went through — write `completed`, catching up with reality) or **post a reversal entry** (if something that already happened is being undone). Only the first is needed here — and if the entry never happened at all, there's nothing to compensate at all, no money ever moved. Reversal entries aren't part of this: they'll be needed in sprint 9 for Stripe refunds, where an already-completed transfer is being undone, rather than its fate being figured out.

### `ledger-svc.GetTransactionByReference` — a source of truth, not a guess

To ask "did the ledger actually execute a transfer with this id," that id has to reach the ledger first. `ExecuteTransferRequest` got an optional `reference` field (`proto/ledger/v1/ledger.proto`) — `transfers-svc` passes `transfer.ID` into it (`settleTransfer` in `transfer.go`), and `ledger-svc` stores it on both entries (`entries.reference UUID`, migration `000004_add_reference_to_entries`, index `idx_entries_reference`). The value is optional and defaults to `NULL` — `cmd/devtopup`/`cmd/seed` never pass it, and that changes nothing about their behavior.

`GetTransactionByReference(reference) → {found, transaction_id}` — `found = false` is just as complete and expected a response as `found = true`, not an error: the reference might never have been used, or a transfer with it might never have executed. Both cases exhaust everything that can possibly be true about a stuck `pending`.

### The worker: `runReconciliationWorker` (`services/transfers-svc/reconcile.go`)

A ticker once every 30 seconds (`reconcileInterval`, a constant — nothing to tune) looks for `pending` transfers older than a configurable threshold (`getStalePendingTransfers`, `transfer.go`) — the threshold is set via `RECONCILE_STALE_AFTER` (`time.ParseDuration`, default `2m`, not set in `docker-compose.yml` — code-only). For each one — `GetTransactionByReference(transfer_id)`:
- **found** → the transfer really did go through: `status = 'completed'`, `ledger_transaction_id` gets filled in. No compensation needed — this is just "catch up with reality."
- **not found** → `ledger` never posted it: `status = 'failed'`, `failure_reason = 'timeout_unresolved'`. No money moved, nothing to compensate.
- a transport error (`ledger-svc` itself unreachable) → nothing is written, just a log line, the next tick tries again — the same fail-closed principle as `checkTransferFraud`/`settleTransfer`: don't know, don't write.

Like the Kafka consumer in `accounts-svc`, the worker lives for the process's entire lifetime with no graceful shutdown (`context.Background()` from `main()`) — the same pattern most background loops in this repo follow. `notifications-svc`'s consumers are the exception, see "notifications-svc: consumer resilience" below.

### A race with the ordinary request handler

Between the moment the worker reads the list of stuck `pending` rows (`getStalePendingTransfers`) and the moment it decides to write one, that same transfer can resolve through the ordinary path — say, the client retried the request with the same `Idempotency-Key`, and this time `settleTransfer` genuinely reached `ledger-svc`. That's why both of the worker's writers — `markTransferCompletedIfPending`/`markTransferFailedIfPending` (`transfer.go`) — are the same `UPDATE`s as `markTransferCompleted`/`markTransferFailed`, but with `AND status = 'pending'` added: if the row is no longer `pending` by the time it writes, the `UPDATE` matches no row at all (`RowsAffected() == 0`), and the worker simply doesn't resolve it again — whatever result was already recorded stays as is, never overwritten by the worker's stale view.

### Logs

Every resolution of a stuck transfer is logged explicitly (`reconcileTransfer`, `transfer.go`) — with the `transfer_id`, the final status, and (for `completed`) the `ledger_transaction_id`:
```
transfers-svc: reconcile: transfer 85abb5f7-... resolved to completed (ledger_transaction_id=0f34b0e1-...) — ledger-svc had already executed it, the original response was never received
transfers-svc: reconcile: transfer d88ce967-... resolved to failed (reason=timeout_unresolved) — ledger-svc never executed it, no money moved
```
A tick that resolves nothing logs nothing — otherwise the log would fill up with noise every 30 seconds for no benefit.

### How the broken connection was simulated to verify this

Neither `docker network disconnect` nor `iptables` can reliably break just the **response** while leaving the actual call and commit inside `ledger-svc` untouched — too fine a timing window to reproduce over a real network. Instead, a temporary `os.Getenv("SIMULATE_CRASH_AFTER_LEDGER_CALL") == "true"` right inside `settleTransfer`, right after a successful `ExecuteTransfer` and before `markTransferCompleted`: `log.Fatalf(...)`, genuinely killing the process at the exact moment `ledger-svc` has already committed but `transfers-svc` hasn't yet. Added, used for the verification below, then removed entirely — a one-time tool for this check, not a permanent part of the code (unlike `cmd/devtopup`, which is genuinely reused).

**Manually verified on the full stack:**
1. `SIMULATE_CRASH_AFTER_LEDGER_CALL=true`, `RECONCILE_STALE_AFTER=5s` (temporary, only for the duration of the check) → restart `transfers-svc`.
2. An ordinary transfer through the Gateway → the client gets a `502` (the connection died along with the process), the `transfers-svc` container is `Exited (1)`, the log shows `SIMULATED CRASH after ledger call for transfer <id>`.
3. `SELECT status FROM transfers WHERE id = '<id>'` → `pending`; `SELECT * FROM entries WHERE reference = '<id>'` → both entries are already there (real money genuinely moved).
4. Remove `SIMULATE_CRASH_AFTER_LEDGER_CALL`, restart `transfers-svc` — the worker runs again. Within one tick (≤35s at a 5s threshold) the transfer became `completed` on its own with the correct `ledger_transaction_id`; the sender's balance (`GET /accounts/me`) dropped by exactly the transfer amount.
5. The opposite case: a `pending` row inserted by hand with no matching entry in `ledger` at all (`INSERT INTO transfers (...) VALUES (..., 'pending', now() - interval '1 minute')`) — became `failed`/`timeout_unresolved` on the next tick.
6. `RECONCILE_STALE_AFTER` and the code were both reverted to the default (`2m`, no variable in `docker-compose.yml`), `SIMULATE_CRASH_AFTER_LEDGER_CALL` was removed entirely from `transfer.go` — an ordinary transfer after reverting is still `completed` on the first try.

### Tests

`services/ledger-svc/ledger_test.go`: `TestExecuteTransfer_WithReference` (the reference is stored on both entries, `getTransactionByReference` finds the correct `transaction_id`), `TestGetTransactionByReference_NotFound`, `TestExecuteTransfer_EmptyReferenceLeavesEntriesUnreferenced` (an empty reference → `NULL`, not an empty string — otherwise every transfer with no reference would collide on the same lookup value).

`services/transfers-svc/reconcile_test.go`: `TestGetStalePendingTransfers` (the age threshold), `TestReconcileTransfer_LedgerExecutedIt`/`_LedgerNeverExecutedIt`/`_TransportErrorLeavesRowUntouched` (three outcomes via `fakeLedgerClient.getTransactionByReferenceFunc`), `TestMarkTransferCompletedIfPending_SkipsAlreadyResolved`/`TestMarkTransferFailedIfPending_SkipsAlreadyResolved` — prove the actual race: resolve a transfer to one terminal status ahead of time, then call the "opposite" `*IfPending` and verify `RowsAffected = 0` and the row is untouched.
```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go test ./services/ledger-svc/... ./services/transfers-svc/... -v
```

## notifications-svc: transfer emails (`transfer.events` → Mailpit)

A third consumer (`runTransferEventsConsumer`, the same `notifications-svc` group, its own reader on the `transfer.events` topic) turns three event types into four kinds of email through Mailpit. Sending uses the same approach as auth-svc: the stdlib `net/smtp`, `Auth = nil`, the body built with `fmt.Sprintf`, no templating engine; the `SMTP_ADDR`/`SMTP_FROM` env vars share the same defaults (`mailpit:1025`, `noreply@neobank.local`), so both services drop mail into the same mailbox. Moving to Brevo/SES means changing these values and adding `smtp.Auth`, not rewriting the logic above.

### Who gets what: three events — four emails

A transfer has two sides, and they learn about different things:

| Event | To the sender | To the recipient |
|---|---|---|
| `TransferCompleted` | "transfer sent" | "transfer received" |
| `TransferFailed` | "transfer failed" | — |
| `TransferRejected` | "transfer declined" | — |

`TransferCompleted` is the only event that produces **two** emails: money left one side and arrived at the other, and both facts matter to their respective recipient. `TransferFailed` and `TransferRejected` mean no money ever moved — there's nothing for the recipient to know about, and their contact isn't even resolved (an extra request plus extra waiting on the projection for an address that would just get thrown away). This also matches sprint 7's decision: a recipient never sees someone else's failed transfers.

Events carry only `sender_account_id`/`recipient_account_id` (UUIDs) — no email, no account number. The address comes from the service's own `user_contacts` projection keyed by `account_id`; for the account number, migration `000003` added an `account_number` column (`AccountCreated` always carried it on the wire, but it went unpersisted before this sprint).

### What's not in the email — and why that's not an oversight

**The email about a fraud-blocked transfer contains neither the rule's name nor its threshold.** `TransferRejected` carries `triggered_rule`, the handler reads it — and writes it only to the log. `buildTransferDeclinedEmail` **physically doesn't accept** such a parameter: naming the rule ("velocity_count") or the limit ("over €5,000.00 in a single transfer") would hand out instructions on how to stay under it. Exactly the same logic as sprint 6's UI (`REJECTED_REASON_LABELS` in `TransferForm.tsx` — a mapping with no fallback to the raw string), except an email is also forwardable and permanent. The missing parameter is what guarantees a future edit can't accidentally leak the rule.

**The recipient's email contains neither the sender's email nor anyone's balance** — `buildTransferReceivedEmail` receives neither. The recipient only needs the amount, the transfer ID, and the account number the money came from.

**The ledger's error codes are never shown raw.** `failureReasonSentences` translates `insufficient_funds` into "There were not enough funds in your account."; an unrecognized code gets no `Reason: ledger_internal_error` string — it gets no `Reason` line at all.

### `event_type` in the Kafka header: a discriminator that doesn't exist in the payload

`transfer.events` is the first topic in the repo carrying several message types, and telling them apart by body is **impossible**. `TransferCompleted`, `TransferFailed`, and `TransferRejected` share the same field numbers and types for fields 1–5, and field 6 is a `string` in all three (`ledger_transaction_id` / `reason` / `triggered_rule`). That means `proto.Unmarshal` of any one of them into any other **succeeds with no error**: a `TransferFailed` read as a `TransferCompleted` silently ends up with `insufficient_funds` sitting in `LedgerTransactionId`. This isn't a hypothetical danger — it's exactly what would have happened with the first naive consumer written against this topic.

The fix is a header, not a proto field: `pkg/outbox/relay.go` now sets `Headers: [{event_type: <outbox.event_type>}]`. The `event_type` column has existed in the outbox table from the start and was only ever spent on logs; carrying it onto the wire in the relay is cheap (three lines in the shared package), needs no protobuf regen, touches not a single producer, and can never drift from what was written in the same transaction as the business change. The header is additive — the `user.events`/`account.events` consumers simply never look at it.

`notifications-svc`, meanwhile, **does not import `pkg/outbox`**: it needs exactly one string constant, and that package is the *write* side (put an event in the outbox in the same transaction as a business change, and relay it), while notifications-svc owns no outbox table and publishes nothing. A pure consumer depending on the publishing library would invert the layers. The literal is duplicated in `kafka.go` and pinned on both sides (`TestHeaderEventType_IsWireContract` in `pkg/outbox`, `TestEventTypeHeader_MatchesProducer` in notifications-svc) — without that, a rename on the producer side would just silently turn off emails.

What the consumer does for each header case (relevant starting with the retry/DLQ introduced below — before that, "the handler returned an error" and "the handler succeeded after a retry" were both just "don't commit," with no boundary around attempts):

| Header | Action | Offset commit |
|---|---|---|
| known type, handler succeeds (immediately or after a retry) | email(s) + `finishEvent` | yes |
| known type, `proto.Unmarshal` fails on every attempt | `transferMaxAttempts` retries, then the DLQ | yes — see "Bounded retry and the DLQ" |
| known type, handler fails on every attempt | `transferMaxAttempts` retries, then the DLQ | yes — see below |
| no header (`""`) | immediately raised as an error, goes through the same retry/DLQ path (deterministically doomed — the header will never appear on any attempt) | yes |
| unrecognized value | the same path as above | yes |

For a headerless message it's tempting to pull `event_id` out by unmarshaling it as `TransferCompleted` (fields 1–5 do line up) — we don't: that's exactly the accidental cross-unmarshal the header exists to eliminate. The DLQ and the log instead carry the partition and offset, which is what an operator would reach for to inspect the message anyway.

### Bounded retry and the DLQ: one broken transfer shouldn't stop the rest

`handleTransferMessageWithRetry` (`kafka.go`) is the boundary that decides "try again" versus "give up and move on," and it's exactly what was missing from the "don't commit" approach described above: `kafka-go`'s `Reader` never re-asks for the same message within a running process — `FetchMessage` always moves forward regardless of whether the previous offset was committed. Before this, that meant message N, followed by a successfully committed N+1, was lost silently the moment the later offset got committed — not an infinite retry, not a blocked partition, just a quiet loss.

Now `processTransferMessage` (unmarshal + dispatch + handler, all as one unit) gets re-invoked for the same message up to `transferMaxAttempts` times (5), with exponential backoff (`transferRetryBaseDelay` = 500ms, doubling, capped at `transferRetryMaxDelay` = 8s). Re-invoking it is safe thanks to `claimEvent`: an attempt that reaches the point of claiming the event leaves the barrier row at `processing` if it then fails, and the next attempt reclaims that same row instead of treating the event as already handled — the same "duplicate beats loss" policy already applied to a crash mid-send now covers a repeatable transient failure too.

If every attempt is exhausted, the message is poison: a payload that will never parse, an address SMTP permanently refuses, or a dependency that stays down for the entire retry window. Instead of it being lost (as an earlier `proto.Unmarshal` failure would have been) or this goroutine hanging on it forever (blocking every notification behind it), the whole message — the same key, the same value, the same original headers — is published to `transfer.events.dlq` (`sendToDLQ`) with `dlq_reason`/`dlq_source_partition`/`dlq_source_offset` headers added; if `event_id` was determined (unmarshal succeeded), the barrier row is closed as `skipped`; the offset commits. Nothing is lost — the DLQ keeps the original bytes for manual inspection; `event_id` isn't always available — a message with no header, or an unrecognized type, has no way to yield one without the exact cross-unmarshal the header exists to prevent, so for these cases no barrier row exists at all, just a DLQ entry and a committed offset.

### A missing contact isn't a poison message

`waitForContactByAccountID`, once its bounded wait is exhausted (~3s, `contactWaitAttempts`×`contactWaitDelay`), returns `found = false` with no error — **not** an error that would land in `handleTransferMessageWithRetry`. This is deliberate: a transfer that outran the projection (`AccountCreated` hasn't been processed yet) is a reason to wait, not a reason to treat the message as broken. A permanently unresolvable `account_id` means a broken projection, not a corrupted payload, and the DLQ — built for "an operator can fix it and replay" — isn't where this belongs. Such an event still commits right away, with a status of `sent`/`skipped` depending on whether at least one side was found (see the table below). A malformed UUID is a different case: Postgres returns `22P02`, which is a genuine handler error, and it correctly falls through the retry/DLQ path.

### The idempotency barrier: `processing` → send → `sent`

Kafka gives at-least-once, and so does the outbox relay (the publish happens before the `published_at` mark) — one event can arrive twice. But **sending an email can't be folded into a DB transaction**: an external side effect doesn't roll back. Exactly-once is unreachable here in principle; only the direction of the error can be chosen.

`claimEvent` (`contacts.go`) is a single atomic statement:

```sql
INSERT INTO notifications_processed_events (event_id, status)
VALUES ($1, 'processing')
ON CONFLICT (event_id) DO UPDATE SET status = notifications_processed_events.status
RETURNING status
```

`DO UPDATE` here is a deliberate no-op: the row is assigned its own existing value. Its only job is to make `RETURNING` fire on the conflict branch; `ON CONFLICT DO NOTHING` returns zero rows and can't report what was already there. The pre-existing status comes back: `sent`/`skipped` → skip, `processing` → do the work.

Why a single statement instead of a pair like `isEventProcessed` + `markEventProcessed`, the way the projection handlers do it: there the side effect is an upsert, and a race between the read and the write is harmless (doing it twice equals doing it once). Here the side effect is an email — it doesn't deduplicate itself, so the check and the claim have to be one atomic statement.

**`processing` meaning "go do the work" is a choice, not a fallback.** A crash between returning from `smtp.SendMail` and `finishEvent` leaves a row that honestly says "unknown whether the email went out." Retrying risks a duplicate; skipping risks silence. For money notifications, the former — a second "you received €25.00" — is a minor annoyance; the latter is a customer who never learns money arrived. **A duplicate beats a loss.**

`finishEvent` closes out the event — specifically an `UPDATE`, and it can't be swapped for `markEventProcessed`: that one uses `ON CONFLICT DO NOTHING`, and since the row already exists after `claimEvent`, it would silently do nothing, leaving `processing` forever and turning every replay into a fresh batch of emails. The most likely way to break this six months from now, which is why the two functions are deliberately kept separate rather than "merged."

There's one thing the barrier doesn't do: **it doesn't serialize concurrent replicas.** Worker A's claim commits right away; worker B, a millisecond later, sees `processing` and also goes to do the work. Unreachable today (a single replica, and Kafka hands a partition to one consumer per group), but this is a property of the "`processing` → do the work" policy, not the SQL, and it's better to know about it in advance.

### One barrier row for two emails — and its cost

`TransferCompleted` has two side effects and **one** row in `notifications_processed_events`. The upside: "was this event processed" is one unambiguous fact. The honest downside: the row can't tell "both sent" apart from "one sent." A crash between the two emails leaves `processing`, and a replay sends **both** — the sender gets a duplicate. That's the same direction ("a duplicate beats a loss"), and the alternative (a barrier row per recipient) would trade it for a scheme where "one row per event" no longer holds.

The sender is emailed first — they're the initiator waiting for confirmation, so if only one of the two emails manages to go out, it should be theirs.

### Ordering with the offset: one attempt versus all five

The offset commits **after** processing, not before — otherwise a crash before sending would lose the event for good. Before retry/DLQ existed, "don't commit" effectively meant almost nothing — `kafka-go`'s `Reader` never re-asks for a message within the process, `FetchMessage` always moves forward, so an uncommitted message was simply lost the moment a later offset got committed. Now every message has a real boundary around its attempts — `transferMaxAttempts` (5) inside `handleTransferMessageWithRetry` — and the commit happens either once one of those attempts reaches `finishEvent`, or once every attempt is exhausted and the message has gone to the DLQ. The difference between "one attempt" and "all five" is what determines the emails and the barrier row's status:

| Situation | Emails | Barrier row | Commit |
|---|---|---|---|
| SMTP is down on attempt #k, k < 5 | none (yet) | `processing` | no — the next attempt fires after `transferRetryDelay(k)` |
| SMTP is down on all 5 attempts | none | `skipped` (closed after the DLQ) | yes — the event goes to `transfer.events.dlq` |
| SMTP blinked, some attempt succeeded | sent | `sent` | yes |
| Email #1 went out, #2 failed, retries exhausted | one (a duplicate is possible on a later retry) | `skipped` (closed after the DLQ) | yes |
| Contact not found within ~3s | whatever resolved | `sent` / `skipped` | **yes**, immediately — not an error, doesn't enter retry/DLQ (see "A missing contact" above) |
| A Postgres error looking up the contact (a malformed UUID, `22P02`) | none (yet) | `processing` | no — the same retry/DLQ path as SMTP |
| `claimEvent` returned `sent`/`skipped` | none | unchanged | yes — the ordinary replay branch |

An unresolved contact commits deliberately, same as before: every account in the system is created by accounts-svc in response to `UserActivated`, so a permanently unresolvable `account_id` means a broken projection, not "an external account" — exactly what "A missing contact isn't a poison message" above explains in more detail. If one of the two sides was found, the email goes out to them (the recipient's email simply loses its *From account* line), status `sent`; if neither was found, no emails go out at all, status `skipped`, meaning "chose not to send," not "couldn't."

### Why `LastOffset`, not `FirstOffset`

The most expensive mistake of this sprint would have lived in a single line of reader config. The readers are split by intent:

- `newProjectionReader` (`user.events`, `account.events`) — `FirstOffset`. `user_contacts` is state: replaying a compacted log rebuilds it, and a repeat upsert costs nothing.
- `newNotificationReader` (`transfer.events`) — **`LastOffset`, and it can't be anything else.** This topic isn't compacted, runs on ordinary time-based retention, and has been accumulating since sprint 5. `FirstOffset` on a new group would replay the entire history, and the idempotency barrier wouldn't stop **a single one** of those events: `notifications_processed_events` has no rows for those `event_id`s. Every user would get an email for every transfer of theirs from weeks back, two for every successful one. An email is a side effect out in the real world, not a state update: it can't be "rebuilt," and history is exactly what must NOT be replayed.

`StartOffset` only takes effect while the group has no committed offset on the partition, so after the first startup it costs nothing — a crash, a restart, and a manual offset reset all resume from the committed position identically. The cost is exactly one skip: a transfer that completed during the very first run, before the first commit, gets no email. Once, on one deploy, in exchange for never spamming an entire history's worth of emails on that same deploy.

### `transfer.events` — `delete`, not `compact`

`kafka-init` now creates `transfer.events` too, with `cleanup.policy=delete` — the broker's default, written explicitly, because the alternative here is actively wrong. This topic's key is `sender_account_id`, and compaction would only keep the **last** event per sender, silently discarding the rest of their transfer history. `user.events`/`account.events` get compacted precisely because they're state snapshots keyed by `user_id`; `transfer.events` is a log of discrete facts, and compacting it would be data loss dressed up as a retention policy.

Pre-creating it instead of relying on auto-creation is also needed because `depends_on: kafka-init` on notifications-svc previously only covered two topics out of three: on a fresh stack with not a single transfer yet, the third reader would hit a nonexistent topic, and `FetchMessage`'s error path was a tight loop with no pause. `fetchErrorBackoff` (1s) was added to all three loops at the same time, for residual cases like a broker restart.

### Amount formatting

`formatMinorUnits` mirrors `frontend/src/features/accounts/money.ts`: `abs/100` and `abs%100` as integer math (never `float64(minorUnits)/100`), manual thousands grouping, exactly two decimal places. `123456` → `1,234.56`. One difference — **no currency symbol**: `formatAmount` appends `" EUR"`, because `€` isn't ASCII, and every email here is ASCII-only — that's exactly what lets it skip a MIME charset header, the same as auth-svc's emails. That assumption is checked by a test, not just assumed.

### Manual verification

A full DoD run-through. transfers-svc needs rebuilding too — the relay that sets the header lives in its process:

```bash
docker compose up -d --build
# register and verify alice@example.com and bob@example.com,
# top up alice's account (see "Dev tools")

# 1. A successful transfer between two users → TWO emails
curl -s -X POST http://localhost:8080/transfers \
  -H "Authorization: Bearer $ALICE_TOKEN" -H "Idempotency-Key: $(uuidgen)" \
  -d '{"recipient_iban":"<Bob's IBAN>","amount":123456}'

curl -s http://localhost:8025/api/v1/messages \
  | jq -r '.total, (.messages[] | "\(.To[0].Address)  \(.Subject)")'
# 2
# bob@example.com    Neo-Bank: transfer received
# alice@example.com  Neo-Bank: transfer sent
#   both bodies show 1,234.56 EUR; alice's has a "To account" line, bob's has "From account"

# 2. Fraud-blocked → ONE email to the sender, with no rule disclosed
curl -s -X DELETE http://localhost:8025/api/v1/messages
curl -s -X POST http://localhost:8080/transfers \
  -H "Authorization: Bearer $ALICE_TOKEN" -H "Idempotency-Key: $(uuidgen)" \
  -d '{"recipient_iban":"<Bob's IBAN>","amount":600000}'
# {"status":"rejected","failure_reason":"amount_threshold"}  <- the API says this; the email shouldn't

curl -s http://localhost:8025/api/v1/messages | jq -r '.total, (.messages[]|.To[0].Address)'
# 1
# alice@example.com          <- the recipient got nothing

curl -s "http://localhost:8025/api/v1/message/<ID>" | jq -r .Text \
  | grep -Ei 'amount_threshold|velocity|500000|threshold|limit'
# (empty — neither the rule's name nor its threshold)
```

A redelivery test — no new emails, one row, still `sent`:

```bash
docker compose exec postgres psql -U neobank -d neobank \
  -c "SELECT event_id, status FROM notifications_processed_events ORDER BY processed_at DESC LIMIT 3;"
#  <the successful transfer's uuid> | sent
#   ^ ONE row, despite TWO emails having been sent

curl -s -X DELETE http://localhost:8025/api/v1/messages
docker compose stop notifications-svc          # the group must be inactive to reset
docker compose exec kafka kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
  --group notifications-svc --topic transfer.events --reset-offsets --shift-by -2 --execute
docker compose start notifications-svc
docker compose logs -f notifications-svc
# notifications-svc: event <uuid> already handled, skipping (redelivery)

curl -s http://localhost:8025/api/v1/messages | jq .total
# 0                                            <- no new emails

docker compose exec postgres psql -U neobank -d neobank \
  -c "SELECT count(*) FROM notifications_processed_events WHERE event_id = '<uuid>';"
#  1
docker compose exec kafka kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
  --describe --group notifications-svc
#  LAG 0 on all three topics — the replay committed, nothing got stuck
```

`--shift-by -2`, not `--to-earliest`: the `transfer.events` reader starts at `LastOffset`, and resetting to the beginning would replay the topic's entire history — exactly what that starting point exists to avoid.

Optionally, "SMTP is down for the entire retry window" — now ends up in the DLQ instead of hanging in `processing`:

```bash
docker compose stop mailpit
# make a transfer → in the log: sendEmailWithRetry (3 attempts), then 5 attempts
# of handleTransferMessageWithRetry with growing pauses (500ms, 1s, 2s, 4s),
# then "giving up ... sending to transfer.events.dlq"
docker compose exec postgres psql -U neobank -d neobank \
  -c "SELECT event_id, status FROM notifications_processed_events WHERE status = 'skipped' ORDER BY processed_at DESC LIMIT 1;"
#  ^ not 'processing' — the row is closed after the DLQ, honestly (we didn't send it, not "we don't know")

docker compose exec kafka kafka-console-consumer.sh --bootstrap-server localhost:9092 \
  --topic transfer.events.dlq --from-beginning --max-messages 1 \
  --property print.headers=true
#  ^ event_type=TransferCompleted, dlq_reason=..., dlq_source_partition=..., dlq_source_offset=...

docker compose start mailpit
curl -s -X POST http://localhost:8080/transfers \
  -H "Authorization: Bearer $ALICE_TOKEN" -H "Idempotency-Key: $(uuidgen)" \
  -d '{"recipient_iban":"<Bob's IBAN>","amount":1000}'
curl -s http://localhost:8025/api/v1/messages | jq .total
#  ^ the next transfer went through fine — the partition isn't blocked by the
#    earlier one that went to the DLQ
```

**On dev data:** for contacts linked before migration `000003`, `account_number` stays `NULL`, and the email simply omits the account line. An offset reset alone doesn't backfill it: barrier rows for those `AccountCreated` events already exist, and every replayed event short-circuits. Either `docker compose down -v`, or (dev-only, by hand — not through code):

```bash
docker compose stop notifications-svc
docker compose exec postgres psql -U neobank -d neobank -c \
  "DELETE FROM notifications_processed_events WHERE event_id IN (SELECT event_id FROM accounts_outbox WHERE event_type = 'AccountCreated');"
docker compose exec kafka kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
  --group notifications-svc --topic account.events --reset-offsets --to-earliest --execute
docker compose start notifications-svc
# account.events is compacted, so the replay delivers the last AccountCreated per user
```

Easiest to demonstrate with fresh users. A cross-service `UPDATE user_contacts ... FROM accounts` is technically possible (one physical database) and deliberately rejected: notifications-svc doesn't read other services' tables.

### Tests

`services/notifications-svc/money_test.go` — a table against `money.ts`'s semantics (`0` → `0.00`, `5` → `0.05`, `123456` → `1,234.56`, `100000000` → `1,000,000.00`, `-2550` → `-25.50`) plus an ASCII check, which is what the missing charset header relies on.

`services/notifications-svc/email_test.go` — this is where the sprint's requirements become executable checks: a declined email's body contains none of `amount_threshold`/`velocity_count`/`velocity_sum`, none of the words `threshold`/`limit`/`rule`, no threshold numbers; a received email contains no `@` (i.e. nobody's email) and no word `balance`, does have the sender's account number, and the *From account* line disappears entirely when the number is empty; a failed email renders every known code as a sentence and omits the `Reason` line for an unknown one, never showing the raw token; a zeroed `occurred_at` never produces a `1970` date.

`services/notifications-svc/kafka_test.go` — `eventTypeOf` (present / absent / empty value / wrong case / duplicate keys → first one wins), and pinning both sets of wire-contract literals.

`services/notifications-svc/dlq_test.go` — no DB needed: `transferRetryDelay` (growth and cap), `sendToDLQ` (key/value/original headers preserved, `dlq_reason`/`dlq_source_partition`/`dlq_source_offset` added, via a fake `kafkaMessageWriter`), and three poison branches of `processTransferMessage` (no header, unrecognized type, `proto.Unmarshal` fails) — none of the three ever touch `pool`, so they're tested with `nil` and no DB.

`services/notifications-svc/contacts_test.go` — against a live database (`t.Skip` with no `DATABASE_URL`, the convention from `pkg/outbox`): `claimEvent`'s lifecycle — the first call returns `true`; **so does the second**, on a row left at `processing` (the crash-recovery policy is verified, not assumed); after `finishEvent(sent)` and `finishEvent(skipped)` — `false`; three claims in a row plus a `finishEvent` leave exactly one row. Plus `getContactByAccountID`: hit, miss, and `account_number IS NULL` → `""`.

`pkg/outbox/relay_test.go` — `TestRelayBatch_StampsEventTypeHeader` (the header reaches the message, exactly once, with the value from the column) and `TestHeaderEventType_IsWireContract`.

```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go test ./services/notifications-svc/... ./pkg/outbox/... -v
```

## notifications-svc: consumer resilience

Retry/DLQ (above) answers "one message shouldn't stop the rest." Here are three related questions: is it visible from the outside when a consumer falls behind or dies; does a restart lose a message that was mid-processing; and does `/healthz` actually tell the truth.

### Consumer lag

`monitorConsumers` (`kafka.go`) logs each of the three readers' lag once every `consumerLagLogInterval` (30s) — `reader.Stats().Lag`, computed by `kafka-go` itself from the partition's high water mark on every fetch. Not `Reader.Lag()` (that returns `-1` in consumer-group mode, and all three readers here have a `GroupID`) — specifically `Stats().Lag`, which per `kafka-go`'s own code updates regardless of mode. Without this, "emails aren't arriving" is indistinguishable from the outside from "nobody's been transferring money": both look like silence in Mailpit. Lag is what tells them apart.

The same numbers are duplicated in `/healthz` (`consumer_lag`, keyed by topic name), not just the log — both forms satisfy the task's requirements ("logs and a simple metric" is enough, Prometheus/Grafana are out of scope), and since the number is already computed for the log, handing it out in JSON too is nearly free. `consumerHealth` in `/healthz` (next section) is updated by a separate loop, not this one — `Stats()` turned out to be the wrong signal for health, even though it works perfectly for lag.

```bash
docker compose logs notifications-svc | grep 'consumer lag'
# notifications-svc: consumer lag: topic=user.events lag=0 offset=42
# notifications-svc: consumer lag: topic=account.events lag=0 offset=17
# notifications-svc: consumer lag: topic=transfer.events lag=0 offset=9

curl -s http://localhost:8086/healthz | jq .consumer_lag
# {"user.events": 0, "account.events": 0, "transfer.events": 0}
```

### Graceful shutdown: SIGTERM finishes the message it's mid-processing

Before this step, not one Kafka consumer in the repo ever shut down any way other than the process being killed — `context.Background()` from `main()`, no signal handling, the exact pattern described (and, here, broken for the first time) in the Reconciliation section above. `notifications-svc` is now the first exception.

`main()` takes a `ctx` from `signal.NotifyContext(..., syscall.SIGINT, syscall.SIGTERM)` and passes it to every consumer as `fetchCtx` — the context `FetchMessage` blocks on. Canceling the context unblocks a reader that's idling while waiting for the next message, and its goroutine exits without starting new work. But a message already pulled from `FetchMessage` at the moment of cancellation is processed on a **separate** `context.Background()`, not `fetchCtx` — otherwise SIGTERM could cut `sendEmailWithRetry`/`claimEvent` off halfway through, leaving the message neither committed nor genuinely processed. That's exactly what "finish the current message" means: the boundary runs per message, not per goroutine. In `runTransferEventsConsumer`, the same logic additionally covers the whole retry chain in `handleTransferMessageWithRetry` — a SIGTERM in the middle of attempt three of five doesn't cut it off, it lets it run to completion (success, the next attempt, or the DLQ).

The HTTP server (`http.Server` instead of a bare `http.ListenAndServe`) stops on the same signal via `srv.Shutdown` with its own timeout (`shutdownTimeout`, 10s) — unrelated to the Kafka side and deliberately time-bounded, unlike the consumers. `main()` waits for all three consumer goroutines via a `sync.WaitGroup` with **no** timeout: cutting that off with a deadline would just reproduce the exact problem graceful shutdown is supposed to remove.

This creates a dependency on an external timeout the code doesn't control: if SMTP is down for the entire retry sequence, draining one message can take longer than the standard 10 seconds Docker/Kubernetes wait between SIGTERM and SIGKILL. `docker-compose.yml` therefore sets `stop_grace_period: 60s` for `notifications-svc` — with room to spare relative to `transferMaxAttempts` attempts and the pauses between them. If a SIGKILL happens sooner anyway (manually verified below, with a deliberately short `--timeout 30` while Mailpit is down), that's not data loss: the message stays uncommitted and `processing`, and `claimEvent` reclaims it on the next startup — the same path that already recovers from an ordinary crash. The generous grace period is what turns this path from "the only one" into "rare, for an external SIGKILL," not a guarantee on its own.

```bash
docker compose stop mailpit                   # to reliably catch a message mid-retry
curl -s -X POST http://localhost:8080/transfers \
  -H "Authorization: Bearer $ALICE_TOKEN" -H "Idempotency-Key: $(uuidgen)" \
  -d '{"recipient_iban":"<Bob's IBAN>","amount":1000}'
docker compose stop --timeout 30 notifications-svc
docker compose logs notifications-svc | tail -20
# notifications-svc: shutdown signal received, draining in-flight work
# notifications-svc: transfer.events: attempt N/5 failed ...
# notifications-svc: waiting for consumers to finish their current message
# ... (retries or the DLQ run to completion, THEN)
# notifications-svc: shutdown complete
docker compose start mailpit notifications-svc
```

### `/healthz`: an honest read on Kafka, not just "the process is alive"

`pkg/health.Handler` (still used by the Gateway) always answers `200` — it checks nothing at all, only that the HTTP server can respond. `notifications-svc` now uses its own inline `GET /healthz` handler instead, like `auth-svc`/`accounts-svc`/`transfers-svc`/`fraud-svc` — but unlike them (which only check `SELECT 1`), this one also checks Kafka: `consumerHealth` (`kafka.go`) is a single `atomic.Bool` for the whole broker (not one per reader — all three readers connect to the same broker list, so "is Kafka reachable" is one fact here, not three). `/healthz` returns `503` if Postgres is unreachable **or** the broker is currently unreachable — previously the service happily answered `200` even if Kafka had been unreachable since startup.

**The flag is updated by `monitorKafkaHealth` — an independent loop that itself dials the broker (`kafka.DialContext`) once every `kafkaHealthProbeInterval` (10s), rather than something derived from the three readers' state.** Getting here meant going through two simpler approaches first, neither of which survived a `docker compose stop/start kafka` check:

1. Update the flag right inside the `FetchMessage` loop (`true` on a fetch error, `false` on success) — honest about failure, but not about recovery: `FetchMessage` doesn't return at all until a message is ready, so a reader that's recovered but idle (no new events) simply has no moment at which the flag could ever flip back. After `docker compose start kafka`, `/healthz` stayed `503` indefinitely.
2. Compare `reader.Stats().Errors` between `monitorConsumers` ticks, on the assumption that `kafka-go` retries the connection in the background regardless of whether `FetchMessage` is blocked, and that this would increment the counter. Verification showed the opposite: with the broker genuinely down, `Stats().Errors` stopped growing after the first few startup errors, even though `FetchMessage` kept explicitly logging `failed to dial` every ~25 seconds — this counter covers a narrower set of internal retry paths than the one a consumer-group reader's dial errors actually travel through.

Both cases were caught the same way: `docker compose stop kafka`, wait, `docker compose start kafka`, wait again **without sending a single message** — and watch whether `kafka` flips back to `true`. A direct, independent probe sidesteps this problem entirely: the result of dialing IS the signal, not something that needs deriving from a reader's internals.

```bash
docker compose stop kafka
sleep 10   # one kafkaHealthProbeInterval cycle
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8086/healthz
# 503
curl -s http://localhost:8086/healthz | jq '{status, kafka, postgres}'
# {"status": "error", "kafka": false, "postgres": true}

docker compose start kafka
sleep 10   # one more cycle — recovery doesn't wait on a message on the topics either
curl -s http://localhost:8086/healthz | jq '{status, kafka, postgres}'
# {"status": "ok", "kafka": true, "postgres": true}
```

## Frontend

`frontend/` is a Vite + React + TypeScript SPA, talking to the backend through the Gateway (`http://localhost:8080`). Routing, the project structure, and a typed API layer are already in place; there are no real forms or screens yet — those are the next steps.

### Running it (dev)
```bash
cd frontend
npm install
npm run dev
```
Brings up a dev server on `http://localhost:5173` (Vite's default port). The backend (the Gateway first and foremost) comes up separately, via `docker compose up`.

### Structure — feature-based, not by file type
```
frontend/src/
├── app/           — routing (react-router), providers (react-query), the layout shell
├── features/
│   ├── auth/      — components/ (LoginPage, RegisterPage), api.ts (register/login/logout/...); hooks/ arrives with the real forms
│   └── accounts/  — components/ (DashboardPage), api.ts (getMe); hooks/ arrives with real UI data-fetching
└── shared/
    ├── ui/          — reusable primitives: Button, Input, Card, tokens.css
    └── api-client/  — the HTTP layer: the fetch wrapper, tokens, single-flight refresh, generated types (see "API client" below)
```
Principle: a feature carries its components, hooks, and API calls together, rather than spread across top-level `components/`, `hooks/`, `api/` directories. `shared/api-client/` is infrastructure only (fetch, tokens, retry logic), not a place for specific endpoint calls: those are typed via the generated types, but live in each feature's own `api.ts`.

Styling is CSS Modules (`*.module.css`), with no separate library: works out of the box with Vite, and the classes are already naturally scoped per component — the same split the feature-based structure already has. Shared tokens (colors, spacing, radius, font) live in `shared/ui/tokens.css`, CSS custom properties with `prefers-color-scheme: dark` support.

### The dev proxy and CORS
The Gateway has no `/api` prefix — its routes are `/auth/*`, `/accounts/*`, etc. directly (`gateway/proxy.go`). The frontend calls `/api/*`; Vite's dev server (`frontend/vite.config.ts`) intercepts `/api/*`, strips the `/api` prefix, and proxies the rest to `http://localhost:8080`. So `GET /api/accounts/me` from the frontend goes out to the Gateway as `GET /accounts/me`.

This eliminates the CORS problem in development entirely: the browser only ever sees one origin (Vite's dev server), and the call to the Gateway happens from the dev server itself, not directly from the browser. **This won't work the same way in production** — there, either the built static assets (`npm run build` → `frontend/dist/`) need to be served through the Gateway itself (putting the frontend and the API back on one origin), or the Gateway needs explicit CORS headers if the frontend and backend stay on separate origins. That choice isn't part of this step.

### API client

**The OpenAPI-spec approach was chosen, not hand-written TS types.** The Gateway's contract (8 auth endpoints + `GET /accounts/me`) is described in `gateway/openapi.yaml`; `frontend/src/shared/api-client/schema.ts` is generated from it by `npm run gen:api` (a wrapper over `openapi-typescript`, see `frontend/package.json`) and **is never edited by hand**. `GET /accounts/{id}` and `PATCH /accounts/{id}/status` were deliberately left out of the spec — the Gateway proxies them, but the frontend never calls them and never will: that's accounts-svc's internal/ops surface, not part of the browser's contract.

Reason for the choice: the spec is also the one live, verifiable description of what the Gateway actually accepts and returns (the request body, every response code, which paths require a bearer token — that's in the spec too, `security` per endpoint is copied straight from `gateway/middleware.go`). Hand-written types would work just as well day to day, but drift from the backend silently: nothing forces anyone to remember them on the next handler change. The cost is an extra generation step on every contract change; with this few endpoints (9), it's worth it.

The typed HTTP methods themselves (`register`, `login`, `getMe`, ...) aren't generated — they're ordinary functions in each feature's `api.ts`, using the `paths[...]` types from the generated schema for every parameter and response. `openapi-fetch` (a typed client built on the same generation) was deliberately not adopted: it takes over parsing the response and wraps the result in `{data, error}`, which doesn't sit well with what `shared/api-client/client.ts` needs to do itself — uniformly throw an `ApiError` (with a status and body) and intercept a 401 for refresh-and-retry. Only what's actually needed from `openapi-typescript` was taken — the types — while all the control-flow logic is hand-written.

```bash
# regenerate types after any change to gateway/openapi.yaml
cd frontend
npm run gen:api
```

`npm audit` at this step shows 2 high-severity findings (a ReDoS in `js-yaml`, a transitive dependency of `openapi-typescript` → `@redocly/openapi-core`). This is a dev-only tool parsing only our own `gateway/openapi.yaml`, not untrusted input — there's no real exposure; `npm audit fix` isn't available yet due to a peer-dependency conflict in `openapi-typescript` on TypeScript (it declares `^5.x`, while the repo is already on `~6.0.2` — the package itself isn't broken by this, only the resolver conflicts).

### Token storage — and its cost

- **The access token** (a JWT, 15-minute TTL) — in memory only, a module-level variable in `shared/api-client/tokenStore.ts`. Doesn't survive a page reload.
- **The refresh token** (opaque, 7-day TTL, single-use — rotated on every `/auth/refresh`) — in `localStorage`, so the session survives a reload.

This is a trade-off, not an oversight. `localStorage` is vulnerable to XSS: any JS injected into the page can read `localStorage` and steal the refresh token, and with it, the ability to reissue the session forever. The genuinely correct fix is an `httpOnly` cookie for the refresh token: then JS (injected or not) physically can't read it — only the browser silently attaches the cookie to requests against `/auth/refresh`. This was deliberately not done at this step, because it requires backend changes (auth-svc would have to respond to `/login`/`/refresh` via `Set-Cookie` instead of a JSON `refresh_token` field, plus a `SameSite`/`Secure` policy, plus the Gateway itself would need to learn to read a cookie, not just the `Authorization` header) — meaning the `TokenPair` contract in `gateway/openapi.yaml` would have to change along with it. The current setup (`localStorage`) is a deliberately accepted short-term trade-off, not how it should stay.

Keeping the access token out of `localStorage` (memory only) is half a mitigation: even a successful XSS can't directly grab a long-lived JWT, only a 15-minute one, and only while the tab stays open. It doesn't fully remove the problem (that same XSS can still call `/accounts/me` as the user while the tab is alive, and pull the refresh token out of `localStorage`), but it narrows both the window and the cost of a compromise.

### Automatic refresh and single-flight

`shared/api-client/client.ts`: any request that gets a `401` automatically calls `/auth/refresh` and retries the original request with the new access token; if the refresh itself fails (rejected by the backend, not just a network blip), the tokens are cleared and `client.ts` does `window.location.href = '/login'`. This is the only thing that triggers a refresh: see the `skipAuthRetry` flag in `RequestOptions` — it's set on every auth endpoint whose own `401` doesn't mean "the token expired" at all (e.g. `/auth/login` with the wrong password), while `/auth/logout` (the one auth path that genuinely needs a session — see `publicPaths` in `gateway/middleware.go`) isn't exempted from the general logic.

The critical part is **single-flight**: `refreshPromise` in `client.ts` is a single promise shared across the module. The first call that catches a `401` creates it and genuinely hits `/auth/refresh`; every other concurrent call sees the already-created promise and waits on it instead of firing its own request. This isn't an optimization, it's a necessity: the refresh token is single-use (rotated on every call, sprint 1) — without single-flight, five parallel requests to `/auth/refresh` would mean only the first one succeeds, and the other four would try to redeem an already-used token and get rejected, logging the user out for no reason. Once it settles (success or failure), `refreshPromise` resets to `null` via `.finally()` — the next, independent expired token (say, 15 minutes later) starts a fresh cycle rather than reusing an already-resolved promise.

**How this was verified.** The manual scenario from the spec (open the dashboard with several parallel requests, watch the Network tab) isn't literally possible yet — real screens and UI data-fetching arrive in later steps, `DashboardPage` is currently a static placeholder. Instead, the behavior was verified with a script driving the real `client.ts`/`tokenStore.ts` under Node (`tsx`) with `fetch`/`localStorage` swapped out: 5 parallel requests, each catching a `401`, and — exactly **one** call to `/auth/refresh`, all 5 successfully retried with the new token. Separately verified that `refreshPromise` doesn't get stuck after the first cycle: a second, independent expired token triggers a fresh (second) call to `/auth/refresh`, not a reuse of the already-resolved promise. The script was temporary (never committed) — once a real dashboard with several requests exists, this check is worth repeating literally, through the Network tab.

### Routes
`/register`, `/login`, `/dashboard` are currently empty placeholder pages (a heading inside a `Card`), only there to verify routing works. `/` redirects to `/login`.

## Load testing (k6 + `loadtest/`)

Sprint 3 already has a correctness-under-concurrency test: concurrent transfers off the same account never push the balance negative (see "Concurrency: a transfer can never push an account negative"). The question here is different — not "does it compute correctly," but "what happens under sustained load": where's the ceiling, what degrades first, and do the invariants hold when the system runs at its limit rather than in a unit test's controlled conditions.

The short answer. The distributed profile's ceiling is **~176 transfers/s**, bottlenecked on **ledger-svc's connection pool**: 16 connections, `pgxpool`'s default, never configured anywhere. The latency floor comes from something else — **four synchronously replicated commits per transfer**, ~20ms each. The hot account gives **31.5 transfers/s and doesn't grow at all** across a 12x increase in concurrency — that's serialization on `SELECT ... FOR UPDATE`, a deliberate cost of the schema, not a bug. Every invariant holds after every run: **0 violations across 53,789 transfers executed**.

### What's measured, and with what

| part | tool | why this specific tool |
|---|---|---|
| load generation | k6 0.55.0 in a container on the `neo-bank_default` network | k6 doesn't need to be installed on the host, and from inside the network it hits `gateway:8080` with no extra host-side loopback |
| fixtures, invariants, watching Postgres | `loadtest/cmd/lt` — a Go utility: `setup`, `fraud`, `probe`, `verify`, `report` | everything k6 is the wrong tool for: it can't create users, can't see inside Postgres during a run, and can't answer the one question this test exists for on a ledger — did the books balance afterward |

The scenario hits `POST /transfers/` **through the Gateway**, not ledger-svc directly. This matters: the bottleneck wasn't known in advance, and measuring the ledger alone would have meant deciding upfront that's where it lives. It did turn out to live there — but that's a result of measuring the full path, not a consequence of only measuring the ledger. The full path of one transfer: 1 Gateway proxy hop, 5 gRPC calls (three into accounts-svc, one each into fraud-svc and ledger-svc), 24 SQL statements, and 4 commits across four separate transactions.

**The trailing slash in `/transfers/` isn't a typo.** The Gateway mounts the route as a subtree (`mux.Handle("/transfers/", ...)`), so Go's `ServeMux` answers a bare `/transfers` with a 301 redirect, and a client that follows the redirect turns its `POST` into a `GET` along the way. The failure is silent and dramatic: the very first run showed **2318 rps, zero errors, and zero cents moved** — that was the transfer history being served up on a `GET`. `common.js` therefore also sets `redirects: 0`, so getting that redirect back immediately surfaces as a wall of `client_error` instead of looking like a record. The frontend knows about the same slash — see the comment in `frontend/src/features/transfers/api.ts`.

### Preparing the run

#### Fraud thresholds are raised — and what does NOT change as a result

fraud-svc's production thresholds (migration `000003`) are `velocity_count > 5` over 300s and `velocity_sum > 1,000,000` over 3600s. Any meaningful load blows through five transfers per sender in the first second, after which everything else gets rejected at the fraud step and never reaches ledger-svc. This isn't a hypothesis: a trial run with four users produced exactly `completed=20, rejected=625` — 4 senders × 5 allowed, then a wall.

`lt fraud -mode loadtest` raises **only** the `threshold_value` on the two velocity rules. `enabled` stays `true`, `window_seconds` stays the same, `amount_threshold` isn't touched at all. This matters: both rules keep running **the exact same two aggregates** against `fraud_checks` on every transfer, in the same window, against the same growing table. The cost of the rule — which is exactly what matters, since fraud-svc was a bottleneck candidate — doesn't change; only the final comparison flips, from "reject" to "approve." The original values are saved to `loadtest/fixtures/fraud-rules.backup.json`, and `lt fraud -mode restore` brings them back.

#### Fixtures are created through the real API

`lt setup` creates N users the public way: `POST /auth/register` → the confirmation code is read out of Mailpit via its HTTP API → `POST /auth/verify-email` → `POST /auth/login` → waiting for the async pipeline (auth-svc's outbox → Kafka → accounts-svc → `AccountCreated` → ledger-svc) to genuinely create the account. This is more expensive than inserting rows into `users` and `accounts` by hand, and buys the one thing hand-inserted rows can't: fixture accounts identical to whatever the system creates for a real user.

The only thing that bypasses the API is funding — there's no public way to issue money into the system, and there shouldn't be. The top-up itself runs through an ordinary `ExecuteTransfer` from genesis (the same locks, the same overdraft check, the same cache update), and only the emission into genesis is a direct, balanced write, exactly as in `cmd/devtopup`. The funding entries are deliberately posted **with no `reference`**: that's exactly what lets the verifier separate "money setup put in" from "money that moved during the run," since every load-test entry is tagged with its own transfer's id.

#### Tokens are reissued before every stage

`run.sh` calls `lt setup -refresh` before every VU level. This isn't extra caution — it's a consequence of an error that actually happened: an auth-svc access token lives 15 minutes, and a full run of three profiles across four stages each takes longer, so tokens expire **mid-run**. This doesn't look like a failure, it looks like a record: the Gateway answers 401 in under a millisecond, and k6 happily reports 2700 rps (twenty times the real throughput) at a zero error rate. One whole profile was lost this way — 164 thousand requests, 151 thousand of them 401s. Now 401 has its own bucket in the classifier, its own "THIS RESULT IS INVALID" line in the k6 summary, and a warning in `REPORT.md`.

### Three profiles

The profiles differ in **exactly one thing**: which sender/recipient pair an iteration picks. The same VU ladder, the same amount, the same request, the same measurement — anything else that differed would be contamination in the experiment.

- **DISTRIBUTED** (`distributed.js`) — 40 users transferring to each other at random. The control: concurrency is spread out, so the ceiling found here is the *pipeline's* ceiling, not a lock's.
- **HOT ACCOUNT** (`hotspot.js`) — everyone transfers into one account (`HOT_DIRECTION=inbound`, the default) or one account transfers out to everyone (`outbound`).
- **DUPLICATES** (`duplicates.js`) — `FANOUT` (default 10) requests sharing the same idempotency key, grouped by k6's global iteration counter, so requests in the same group go out from different VUs within milliseconds of each other. The sender, recipient, and amount are derived from the group index rather than chosen at random: within a group they must match, or `reconcileReplay` returns a 422 "key reused with different parameters" and the wrong branch gets exercised.

### How to run it

```bash
docker compose up -d

# fixtures: 40 users with funded accounts (idempotent, safe to rerun)
go run ./loadtest/cmd/lt setup -users 40 -fund 100000000

# raise fraud thresholds for the duration of the runs
go run ./loadtest/cmd/lt fraud -mode loadtest

# profiles: each runs at 10/30/60/120 VUs, with a Postgres probe alongside
# and an invariant check at the end
./loadtest/run.sh distributed
./loadtest/run.sh hotspot
./loadtest/run.sh duplicates
./loadtest/run.sh all      # or all of them at once

# restore the production thresholds
go run ./loadtest/cmd/lt fraud -mode restore
```

Results land in `loadtest/results/`: `<profile>-vus<N>.summary.json` from k6, `<profile>-vus<N>.probe.csv` from the probe, `<profile>.verify.json` from the invariant check, and a combined `REPORT.md` from `lt report`. Knobs: `VUS="10 50" DURATION=30s`, `HOT_DIRECTION=outbound`, `FANOUT=25`, `AMOUNT=...`. On Windows the script is run from Git Bash — it converts the path itself via `pwd -W`, because Docker's bind-mount wants a Windows-style path.

### Results

40 accounts, 100,000,000 minor units each, a transfer of 100. Every stage runs 60 seconds. Latency is the full round trip of `POST /transfers/` through the Gateway.

| profile | VU | RPS | executed/s | p50 | p95 | p99 | max | errors |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| distributed | 10 | 59.7 | 59.7 | 166ms | 208ms | 237ms | 386ms | 0% |
| distributed | 30 | 151.0 | 151.0 | 188ms | 280ms | 353ms | 523ms | 0% |
| distributed | 60 | **176.4** | 176.4 | 327ms | 464ms | 546ms | 760ms | 0% |
| distributed | 120 | 171.9 | 171.9 | 683ms | 847ms | 942ms | 1070ms | 0% |
| hot account | 10 | 31.6 | 31.6 | 314ms | 405ms | 490ms | 657ms | 0% |
| hot account | 30 | 31.5 | 31.5 | 938ms | 1136ms | 1477ms | 2137ms | 0% |
| hot account | 60 | 31.5 | 31.5 | 1893ms | 2180ms | 2386ms | 2962ms | 0% |
| hot account | 120 | 32.1 | 32.1 | 3778ms | 4016ms | 4312ms | 4988ms | 0% |
| duplicates | 10 | 234.3 | 23.4 | 37ms | 138ms | 167ms | 791ms | 0% |
| duplicates | 30 | 507.4 | 50.8 | 43ms | 203ms | 260ms | 435ms | 0% |
| duplicates | 60 | 540.0 | 54.0 | 96ms | 260ms | 321ms | 514ms | 0% |
| duplicates | 120 | 560.0 | 56.0 | 200ms | 363ms | 434ms | 589ms | 0% |

The table deliberately omits average latency: it hides the tail. The gap between "p50 683ms" and "avg 695ms" on the distributed profile at 120 VUs looks negligible right up until you look at the p99 of 942ms.

Outcome breakdown: across all twelve runs, **not a single** 5xx, not one dropped connection, not one `failed`, not one `rejected`, not one 202 "outcome unknown." The only two outcomes that ever showed up were `completed` and `replayed` (in the duplicates profile). The full breakdown is in `loadtest/results/REPORT.md`.

#### Distributed: a knee between 30 and 60 VUs

Throughput plateaus around **176 transfers/s**, and beyond that every added unit of concurrency turns entirely into queueing: from 60 to 120 VUs, RPS doesn't grow (it even dips slightly, 176.4 → 171.9), while p50 doubles, 327 → 683ms. A Little's Law sanity check confirms the measurement is internally consistent: 171.9 × 0.695s = 119.5 ≈ 120 VUs.

#### Hot account: 31.5 transfers/s, independent of worker count

The exercise's central result, because it's the same to three significant figures across the whole range:

| VU | RPS | p50 |
|---:|---:|---:|
| 10 | 31.6 | 314ms |
| 30 | 31.5 | 938ms |
| 60 | 31.5 | 1893ms |
| 120 | 32.1 | 3778ms |

Concurrency grew 12x, throughput grew not at all, latency grew by exactly 12x. Every added worker converts one-to-one into waiting, and delivers not a single extra transfer.

The cause isn't a mystery, it's a direct consequence of the schema. `executeTransfer` takes `SELECT ... FOR UPDATE` on both `ledger_accounts` rows before checking the balance, and holds them until the transaction's end, so every transfer touching the hot row queues up behind the last one. The hot row's ceiling equals `1 / (lock hold time)`; from the measured 31.5 transfers/s that works out to **31.7ms** of hold time. That's **18% of the distributed profile's ceiling** (176.4 → 31.5).

The probe confirms the mechanism independently of latency: `lock_waiters` on the hot profile caps at exactly **15** and never goes higher — 15 sessions waiting on the lock, the 16th holding it, ledger-svc's entire pool spent on one row.

**This is the chosen schema's expected behavior, not a defect.** This exact lock is what guarantees "an account can never go negative" under concurrency (sprint 3). The alternatives — balance sharding on counts, optimistic locking with retries, netting — trade away either that guarantee or the schema's simplicity. The honest way to state the result: *the hot account gives 31.5 tx/s because of row-lock serialization; this is a deliberate limitation of the chosen schema, measured, not assumed.*

Worth noting too is a boundary the run came right up against: at 120 VUs the max latency is 4988ms, and `ledgerCallTimeout` in transfers-svc is 5 seconds. One more step up, and `settleTransfer` would start getting `DeadlineExceeded`, leaving transfers `pending` and returning 202s, handing the work off to the reconciliation worker. The degradation would be correct (that's exactly what the "outcome unknown" branch exists for), but it wouldn't have grown the hot row's throughput any.

#### Duplicates: exactly 9.00 replays per transfer executed

| VU | RPS | executed | replays | replays per transfer |
|---:|---:|---:|---:|---:|
| 10 | 234.3 | 1408 | 12,670 | **9.00** |
| 30 | 507.4 | 3053 | 27,469 | **9.00** |
| 60 | 540.0 | 3250 | 29,241 | **9.00** |
| 120 | 560.0 | 3378 | 30,400 | **9.00** |

At `FANOUT = 10`, the ideal outcome is one winner and nine replays per key. That's exactly what happened, at all four stages, with no deviation across a 12x growth in concurrency. Not one 422 ("key reused"), not one duplicate, not one 5xx.

Total RPS is higher here (560 versus 176), because nine requests out of ten take the cheap path: `createTransfer` finds the existing row by key and returns through `reconcileReplay`, never reaching fraud-svc, never reaching ledger-svc, never hitting a single commit. That's exactly why "executed/s" is low in this profile — only one request in ten does real work.

And most importantly: **this profile proves nothing without post-run verification.** The failure it's hunting for is two requests with the same key, both reaching ledger-svc: that would produce **one** row in `transfers` and **four** entries, the books would still balance, no constraint would be violated, and every HTTP response would look flawless. Only `transfer_entries_paired` (below) catches this.

### Bottleneck #1 — ledger-svc's connection pool (16 of them, set by nobody)

The slowest request at 120 VUs, as it looks in Jaeger (trace `3b77ed24…`, 841ms):

```
+   0.0ms  840.8ms  gateway        POST /transfers/
+   1.1ms  839.1ms  transfers-svc  POST /
+  11.3ms   37.3ms  transfers-svc  query INSERT          <- the transfers row, autocommit
+  48.6ms   29.4ms  transfers-svc  fraud/CheckTransfer
+  78.0ms  740.9ms  transfers-svc  ledger/ExecuteTransfer
+  78.6ms  441.1ms  ledger-svc     pool.acquire          <- WAITING ON A CONNECTION, 441ms
+ 520.3ms  247.0ms  ledger-svc     query SELECT          <- waiting on the row lock
+ 793.2ms   25.3ms  ledger-svc     query COMMIT
+ 821.4ms   18.8ms  transfers-svc  query COMMIT
```

**441 of the 841 milliseconds is waiting for a free connection in ledger-svc's pool, before a single query even runs.** The `pool.acquire` span exists because `pkg/pgha.NewPool` wires `otelpgx` into every pool in every service (the "Tracing" sprint), and it's exactly what turns "slow for some reason" into "this specific queue, right here."

The pool size is set nowhere. `pgxpool` defaults to `max(4, runtime.NumCPU())`, and not one DSN anywhere in the repo passes `pool_max_conns` — on this machine (16 CPUs on the Docker VM), that comes out to **16 connections per service**. Confirmed independently: `lock_waiters` on the hot profile caps at exactly 15.

Worth calling out separately: **since the size is derived from the core count, it depends on the machine.** The same image on a 4-core node gets a pool of 4 connections and hits this ceiling four times sooner. An unset pool size isn't "a sensible default," it's an implicit capacity limit that changes the moment you move to different hardware. Postgres's own `max_connections` is 200 here (`infra/patroni/patroni.yml`), meaning the server isn't the bottleneck at all — it's narrow on the application side.

### Bottleneck #2 — four synchronously replicated commits per transfer

This isn't a ceiling, it's a **floor**: the point latency can't drop below even with a single user. One transfer is four independent transactions in four places: the `INSERT` of the `transfers` row (autocommit); fraud-svc's transaction with `INSERT fraud_checks`; ledger-svc's transaction (two locks, a balance check, 2 entries, 2 cache updates); the `UPDATE transfers` + `INSERT outbox` transaction.

In a calm trace (10 VUs, 126ms total), the four commits cost 30.8 + 18.4 + 24.2 + 19.3 = **92.7ms, or 74% of the entire request.** The commit's cost is nearly independent of the write's size: committing a single row in `fraud_checks` costs about as much as committing six rows in the ledger. That's what a fixed round-trip fee looks like, not a fee for work done.

That fee is `synchronous_commit: on` together with `synchronous_mode: true` from the Patroni sprint: the leader doesn't confirm a commit until a synchronous standby has flushed its WAL. The probe measures this directly — `sync_flush_lag_seconds`, pulled from `pg_stat_replication` on the row with `sync_state = 'sync'`, peaks at **27.9ms**, matching the observed commit cost.

A deliberate cost, not a defect: this exact setting is the one thing that makes automatic failover safe for the ledger (without it, a promoted node might not know about a transaction the client was already told committed, and `SUM(entries)` stops being zero — see the reasoning in `infra/patroni/patroni.yml`). Trading it away for throughput would mean trading "the bank never loses confirmed money" for a nicer-looking number.

### Bottleneck #3 — the outbox relay: 1.0 events a second

The most unexpected finding, and it only surfaced because the probe measures the `outbox`, not just HTTP latency. Measured with no load at all, a clean drain of the accumulated backlog (240s):

```
t= 34s  backlog 1129 -> 1029   (-100)
t=134s  backlog 1029 ->  929   (-100)
t=234s  backlog  929 ->  829   (-100)
longest open transaction over the window: 99.5s
```

Exactly 100 events every exactly 100 seconds. **1.0 events/s** — while the distributed profile writes to the outbox at close to 176 events/s. Over one run of the distributed profile alone (four stages, 4 minutes under load), the backlog grew from 1,140 to 34,216 rows, and it never drains again.

The mechanism is precise, not assumed. `outbox.RelayBatch` grabs a batch (`DefaultBatchSize` = 100) in **one** Postgres transaction and publishes events **one at a time**, a separate `writer.WriteMessages` call for each. `kafka-go`'s `Writer.BatchTimeout` defaults to **1 second**, and the synchronous writer waits until either the batch fills (100 messages) or the timeout expires; one message per call means every single one pays the full second. Hence 100 seconds for a batch of 100 — and hence the 99.5-second open transaction visible in `longest_txn_seconds`.

The side effect is worse than the delay itself: a transaction held open for a minute and a half holds back the `xmin` horizon for that entire time, blocking vacuum across the **entire** database, not just `outbox`.

The practical consequence for the system: transfer emails (`transfer.events` → notifications-svc) lag behind the transfers themselves by this entire delay. No data is lost — that's the whole point of the outbox — but "sent a transfer, got an email" turns into hours under load.

### Bottleneck #4 — the overdraft check sums the entire journal, while holding a lock

`executeTransfer` checks sufficient funds like this:

```sql
SELECT COALESCE(SUM(amount), 0)::bigint FROM entries WHERE ledger_account_id = $1
```

— i.e. it sums the account's **entire** entry journal, already holding `FOR UPDATE` on it. Measured on the live database after the runs (the leader, a warmed cache, best of 5):

| entries on the account | `SUM(entries)` |
|---:|---:|
| 1 | 0.019ms |
| 50 | 0.039ms |
| 355 | 0.053ms |
| 2,408 | 0.869ms |
| 10,003 | 1.599ms |

On a cold cache and under load, the same query on an account with 9,438 entries took 6.2ms. The growth is measured with confidence, but extrapolating these points into "this much at a million entries" isn't valid: across the measured range the relationship is noticeably sublinear (page density, caching), and it's more honest to say "grows with the account's history" than to name a coefficient.

What matters more — **this time falls inside the lock's hold window**, meaning it directly lowers the hot row's ceiling, and it grows exactly with the load that account has already absorbed. A hot account gets slower the more has passed through it.

The irony is that the number it needs is already sitting right next to it: `account_balances` is updated by the same function, in the same transaction, under the same lock, and `getBalance` reads the balance from exactly there. The overdraft check is the one remaining place that still sums the journal.

### What turned out NOT to be a bottleneck

**fraud-svc.** The task description flagged the velocity rules as a likely bottleneck: they run history queries on every transfer. Checked — didn't hold up. In the traces, the two `fraud_checks` aggregates cost 0.6ms each, while all of `CheckTransfer` is 21–29ms, of which 18–25ms is its own `COMMIT`. In other words, fraud-svc is expensive for exactly the same reason as everything else — synchronous replication on the write to `fraud_checks` — not because of its own rules. The reason it's cheap is concrete: `idx_fraud_checks_account_id_created_at` covers `(account_id, created_at)` — exactly the predicate both queries use — and the 300/3600-second window caps the result set from above independent of how large the table grows. A negative result deserves to be written down just as explicitly as a positive one.

**Replica lag.** Under write load it stays microscopic: `replica_lag_seconds` peaks at 0.05s, and in bytes, hundreds of kilobytes. Expected under `synchronous_mode`: a standby can't fall far behind, because the leader is waiting on it. Lag isn't a problem here — it *is* bottleneck #2, just measured from the other side.

**What actually fell over first was Jaeger.** `jaeger all-in-one` stores traces in memory, sampling is 100% locally, and after ~60 thousand traced transfers the container was using **13.3 of 15.5 GiB** of host memory. The resulting memory pressure caused DNS timeouts inside the Docker network and a brief Patroni outage. The README already says 100% sampling is local-only (see "Sampling"); now there's a number backing that claim. The practical takeaway for the runs themselves: restart Jaeger before a long series, or the first thing to degrade won't be the bank, it'll be the thing watching it.

### Invariants — without them, the test proves nothing

After every profile, `lt verify` runs eight database checks. Four come straight from the task description; the other four were added because without them, the first four can pass while the system is still broken.

| check | what it asserts |
|---|---|
| `entries_sum_zero` | `SUM(entries.amount)` across the **entire** table = 0. The one deliberately not scoped to the cohort: a bug that unbalanced the books could have landed on genesis too |
| `no_negative_balances` | no cohort account ever went below zero (genesis and the external-world account are negative by construction — that's what emission looks like) |
| `transfer_entries_paired` | every completed transfer has **exactly two** entries carrying its `reference`, summing to zero, a debit on the sender, a credit on the recipient, both equal in magnitude to the transfer amount |
| `balance_delta_matches_transfers` | per account: "how much should have moved, per the `transfers` table" = "how much the ledger actually moved." The numbers come from **different tables, written by different services**, so this is a genuine reconciliation, not a tautology |
| `no_duplicate_idempotency_keys` | one `transfers` row per key |
| `balance_cache_matches_log` | `account_balances` = `SUM(entries)` per account. The cache updates via read-modify-write under a lock; if the lock ever grabbed the wrong row, the journal would stay correct while the cache drifted, and the API shows the cache |
| `no_entries_for_failed_or_rejected` | no money moved for any transfer recorded as failed or rejected. `pending` is deliberately excluded: a pending row with entries is a legitimate outcome of the "outcome unknown" branch, resolved by the reconciliation worker |
| `cohort_money_conserved` | the cohort's total balance equals what setup put into it. Every test transfer stays inside the cohort, so no matter how many ran or how tangled they got, the sum has to stay the same |

Result: **all eight checks pass after every one of the three profiles.** On the final run — 53,789 transfers executed, 87,888 entries, 0 violations.

Two of the checks deserve a separate note.

**`no_duplicate_idempotency_keys` is nearly a tautology** — `idempotency_key` carries a UNIQUE constraint, so it can only fail if someone drops that constraint in a migration. It's kept specifically for that case. The real check on duplicate protection is `transfer_entries_paired`.

**`no_negative_balances` scopes its failure to the cohort and only reports on everything else.** The dev environment accumulates accounts from past experiments — three turned up here, with negative balances left over from refund and failover tests, all dated before this directory even existed. A check that fails on data it never created is a check whose output gets skimmed past within a week. The load test only ever moves money between cohort accounts, so scoping the failure to the cohort loses nothing, and other accounts' negative balances still print, explicitly flagged as unrelated.

### Slow-request traces in Jaeger

The connection to sprints 3–4 works, and degradation can be dug into without guessing:

```bash
# every transfer slower than 500ms in the last 5 minutes
curl -s "http://localhost:16686/api/traces?service=transfers-svc&operation=POST%20%2F&limit=100&lookback=5m&minDuration=500ms"
# or in the UI: http://localhost:16686 -> service=gateway, operation=POST /transfers/, Min Duration=500ms
```

What's visible here exists purely because the instrumentation was already in place:

- `pool.acquire` as its own span (`otelpgx` is wired into `pkg/pgha.NewPool` for all seven pools) — this is what immediately splits "slow" into "waiting on a connection" versus "waiting on a lock," instead of leaving a black hole inside the handler. Bottleneck #1 was found exactly this way;
- every `BEGIN`/`COMMIT` as its own span — this is exactly what made it visible that 74% of a calm request is four commits;
- `neobank.outbox.lag_ms` on the publish span — the relay's delay is answered straight from the trace, with no query against the DB.

### What was deliberately not done

- **ledger-svc's pool was not enlarged.** The most obvious and cheapest fix (`?pool_max_conns=N` in the DSN) — and exactly why it deserves to be done separately and deliberately: 16 connections × 7 services is already 112 against `max_connections = 200`, so "just raise it" isn't possible without also deciding who gets how many, and whether pgbouncer is needed.
- **The hot-row serialization was left untouched.** Removing it means changing the schema: sharding the balance across counters, moving to optimistic locking with retries, or netting. All of these trade away either simplicity or the guarantee itself that "an account never goes negative." That's an architecture rework, not an optimization.
- **`SUM(entries)` was not replaced with reading `account_balances`** in the overdraft check, even though the number it needs sits right there, updated by the same transaction. That swap moves the source of truth for "is there enough money" from the journal to a cache, and that's not a decision to make in passing, inside a load-testing task, for a ledger.
- **The outbox relay was not fixed.** The fix is small (set `BatchTimeout`, or assemble the batch into a single `WriteMessages` call), but `pkg/outbox` is shared by three services, and its guarantees rest on the ordering "publish first, then mark" — with a batched write, "partially sent" stops being an impossible state, and the marking logic after a partial failure needs to be rethought and covered by a test. Its own task, with its own test.
- **`synchronous_commit` was not turned off.** That would at least double throughput and cancel out the exact guarantee the whole Patroni sprint was built around.

### Caveats on methodology

- **The load generator lives on the same machine** as the system: the k6 container and all 15 stack containers share one Docker VM with 16 CPUs. At 120 VUs, part of the measured latency is contention for those same cores. The absolute numbers characterize "this stack on this laptop," not production capacity; comparisons between profiles are fair, because all three ran under identical conditions.
- **Postgres is a three-node cluster with synchronous replication on Docker Desktop under Windows**, where `fsync` is noticeably more expensive than on a real disk. This inflates bottleneck #2 above all else.
- **One run per stage, no warmup, no repeats.** For the shape of a curve (the hot account's plateau is visible to three significant figures), that's enough; for a claim like "got 5% faster," it isn't.
- **The absolute numbers depend on database volume**, which grew as the runs went on: bottleneck #4 is a direct consequence. The profiles ran in order distributed → hotspot → duplicates against one database, so the later ones ran against a fuller `entries` table than the earlier ones.
- **`RECONCILE_STALE_AFTER=20s`** in `.env` (a demonstration value in place of the 2-minute default) means the reconciliation worker runs more aggressively than usual during the runs, adding its own share of queries.
- **The duplicates profile was rerun** after its first attempt hit expired tokens (see "Tokens are reissued before every stage"), and Jaeger was restarted before the rerun to free up memory. The distributed and hot-account profiles were unaffected by this — both completed comfortably within a token's lifetime, confirmed by zero 401s in their outcome breakdowns.

## Status
At this step, only the repository structure and `docker-compose.yml` are described.
The next steps will add the services' Go code, infrastructure integration (Postgres/Redis/Kafka), and CI.
