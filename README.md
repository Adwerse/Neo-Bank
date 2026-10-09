# Neo-Bank

Neo-Bank is a small retail bank you run on your own machine. Customers register with an email code, top up with a Stripe test card, send money to another customer's IBAN, and watch both balances change without reloading the page. Behind the UI are seven Go services, one gateway, Kafka, and a three-node Postgres cluster with automatic failover.

The project exists to answer engineering questions with measurements rather than assumptions. Can two concurrent transfers push an account negative? What happens when the same request arrives twice? What does a customer see when the database leader dies mid-transfer? Where does throughput stop? No real money moves, and the IBANs carry a fictional bank code (`ZZZZ`).

[![CI](https://github.com/Adwerse/Neo-Bank/actions/workflows/ci.yml/badge.svg)](https://github.com/Adwerse/Neo-Bank/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](go.work)
[![React](https://img.shields.io/badge/React-19-149ECA?logo=react&logoColor=white)](frontend/package.json)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

<img src="docs/screenshots/dashboard.png" alt="Desktop dashboard for Maya Walsh: balance €2,320.25, a balance trend chart, account details with IBAN IE13 ZZZZ, money flow of €82.75 in and €162.50 out, quick actions, and five operations including a rejected €6,000.00 transfer." width="100%">

[Demo script](DEMO.md) · [Engineering log](spec.md) · [Load test report](https://claude.ai/code/artifact/b40504bd-656e-452a-bb32-3e4ec344bd26) · [Development diary](DEVLOG.md)

## What the system guarantees

Each row is a property the code enforces, the mechanism that enforces it, and the test or measurement that checks it.

| Guarantee | Mechanism | Evidence |
| --- | --- | --- |
| An account never goes negative, even under concurrent transfers | `SELECT … FOR UPDATE` on both ledger rows, in ascending id order, before the balance check | 20 concurrent transfers against a 10,000-unit account: exactly 10 succeed ([test](services/ledger-svc/ledger_test.go#L655)) |
| A deposit or refund delivered twice is posted once | `pg_advisory_xact_lock` on the operation's reference | 20 concurrent calls sharing one reference produce one ledger entry ([test](services/ledger-svc/ledger_test.go#L965)) |
| A retried transfer with the same idempotency key moves money once | Unique key on `transfers`; a replay returns the stored row | Load test, duplicates profile: exactly 9.00 replays per executed transfer at 10, 30, 60 and 120 virtual users |
| A crash cannot drop an event, or publish one for a change that never committed | Transactional outbox: the event row is written in the same Postgres transaction as the change | [`outbox.go:84`](pkg/outbox/outbox.go#L84), relay and cleanup tests in `pkg/outbox` |
| A transfer whose outcome is unknown is resolved from the ledger, not guessed | It stays `pending`; a worker asks `ledger-svc.GetTransactionByReference` | Run for this README: fraud-svc stopped, transfer answered `202 pending`, worker closed it as `failed / timeout_unresolved` 27 s later |
| A fraud-service outage never lets a transfer through unchecked | Fail-closed: no fraud decision means no ledger call | Same run: the money did not move while fraud-svc was down |
| Killing the Postgres leader loses no confirmed transaction | Patroni `synchronous_mode` and HAProxy routing by Patroni role | [`failover_test.go`](infra/failover/failover_test.go#L424): four `docker kill` runs, 23.6–25.4 s of write downtime, every confirmed pair present on the new leader |
| A balance on screen is never a stale value pushed over the socket | The WebSocket carries only `balance.changed`; the client re-fetches over HTTP | [`notify.go:55`](gateway/notify.go#L55) |

The repository has 251 Go test functions in 37 files. They run against a real Postgres with no database mocks, and they skip themselves when `DATABASE_URL` is unset. CI builds and vets every workspace module; the integration tests need the compose stack and run locally.

## Product tour

The screenshots and the animation come from a local run on 9 October 2026 with two test users, Maya and Liam. Desktop widths get the light theme and phone widths the dark one; the viewport picks the theme, not the OS setting.

### One transfer, two phones

<img src="docs/screenshots/live-transfer.gif" alt="Two phones side by side. On the left Maya confirms a €42.50 transfer. On the right Liam's balance moves from €770.00 to €812.50, a new row appears in his recent operations and a toast reads Incoming transfer: +€42.50." width="100%">

Maya confirms €42.50 on the left. Liam's phone receives a `balance.changed` signal over the WebSocket, fetches the new balance and history over HTTP, and shows a toast. Nobody reloads anything.

### Onboarding

| Register | Verification code, delivered to Mailpit |
| --- | --- |
| <img src="docs/screenshots/register.png" alt="Registration form with email maya.walsh@example.org and two password fields." width="360"> | <img src="docs/screenshots/verification-email.png" alt="Mailpit showing the email Verify your Neo-Bank email with a six-digit code that expires in ten minutes." width="100%"> |

auth-svc sends the six-digit code over real SMTP to Mailpit, a local inbox, so no email leaves the Docker network. Verifying the code proves ownership of the mailbox and nothing more; there is no KYC.

### Money in, money moved

| Card top-up, Stripe test mode | Transfer confirmation |
| --- | --- |
| <img src="docs/screenshots/deposit-card.png" alt="Pay by card step with Stripe's Payment Element: card number 4242 4242 4242 4242, expiry 12/34, CVC 123, country Ireland." width="360"> | <img src="docs/screenshots/transfer-review.png" alt="Confirm transfer step: recipient IE42 ZZZZ 0000 4373 7769 08, sending −€120.00, remaining balance €2,280.00, buttons Confirm and send and Edit." width="400"> |

A deposit has two separate facts with two statuses: Stripe confirmed the card charge (`succeeded`), and the ledger credited the money (`credited`). The UI says "Payment accepted, crediting within a minute" until the second fact is true. The accounts in these screenshots were funded with [`cmd/devtopup`](services/ledger-svc/cmd/devtopup/main.go) instead, because Stripe's checkout runs an invisible hCaptcha that an automated browser cannot pass. The full deposit flow, from PaymentIntent through webhook to the credit worker, and its manual verification are in [spec.md → Stripe-funded deposits](spec.md#stripe-funded-deposits-transfers-svc-ledger-svc).

<img src="docs/screenshots/recipient-live-update.png" alt="Liam's desktop dashboard a moment after receiving €120.00: a toast in the top right, balance €770.00, money flow showing incoming transfers of €120.00, and the completed transfer in the operations table." width="100%">

Liam's desktop dashboard a moment after Maya's €120.00 transfer: the toast, the new balance, the money-flow bar and the operation row all arrived without a reload.

### Fraud check, profile, mobile

| A blocked transfer | Profile |
| --- | --- |
| <img src="docs/screenshots/fraud-rejected.png" alt="Transfer card with a warning: This transfer was blocked by our security system to protect you. Reason: the amount exceeds the limit for a single transfer. Below it the history shows a rejected −€6,000.00 transfer." width="420"> | <img src="docs/screenshots/profile.png" alt="Profile page with initials avatar M, name Maya Walsh, email, IBAN, account number and an Active status badge." width="400"> |

€6,000.00 is over the `amount_threshold` rule (€5,000.00). fraud-svc names exactly one rule per decision, the API returns `rejected / amount_threshold`, and ledger-svc is never called. The recipient is not told the transfer existed.

<table>
  <tr>
    <td align="center"><img src="docs/screenshots/mobile-dashboard.png" alt="Mobile dashboard in the dark theme: balance €2,320.25, balance-over-time chart, IBAN, quick actions, money flow and recent operations." width="290"></td>
    <td align="center"><img src="docs/screenshots/mobile-history.png" alt="Mobile transfer screen in the dark theme with the IBAN and amount form above an operation history with filters for type, status and time." width="290"></td>
  </tr>
  <tr>
    <td align="center">Mobile dashboard</td>
    <td align="center">Transfer form and history with filters</td>
  </tr>
</table>

## Architecture

The browser talks only to the Gateway. The Gateway validates the JWT, sets `X-User-Id`, and proxies by path prefix. Services call each other over gRPC. ledger-svc has no HTTP API and is unreachable from the Gateway.

```mermaid
flowchart TB
    browser(["Browser · React SPA"]) -->|"HTTPS + WebSocket"| gw["Gateway<br/>JWT · routing · WS push"]
    gw --> auth["auth-svc"]
    gw --> accounts["accounts-svc"]
    gw --> transfers["transfers-svc"]
    transfers -->|"gRPC"| accounts
    transfers -->|"gRPC"| fraud["fraud-svc"]
    transfers -->|"gRPC"| ledger["ledger-svc"]
    accounts -->|"gRPC"| ledger
    transfers <-->|"REST + webhook"| stripe(["Stripe test mode"])
    auth --> redis[("Redis")]
    auth --> minio[("MinIO")]
    notif["notifications-svc"] -->|"SMTP"| mailpit(["Mailpit"])
    auth & accounts & transfers & fraud & ledger & notif --- pg[("Postgres 16, three nodes<br/>Patroni + etcd · HAProxy")]
```

Events travel the other way, asynchronously. A producer writes the event into an outbox table in the same transaction as the business change, and a relay publishes it to Kafka afterwards.

```mermaid
flowchart LR
    auth["auth-svc"] -.->|"outbox"| ue[["user.events<br/>compacted"]]
    accounts["accounts-svc"] -.->|"outbox"| ae[["account.events<br/>compacted"]]
    transfers["transfers-svc"] -.->|"outbox"| te[["transfer.events"]]
    ue --> accounts
    ue --> notif["notifications-svc"]
    ae --> notif
    ae --> gw["Gateway"]
    te --> notif
    te --> gw
    notif -->|"SMTP"| mailpit(["Mailpit"])
    notif -.->|"after 5 attempts"| dlq[["transfer.events.dlq"]]
    gw -->|"balance.changed"| ws(["browser WebSocket"])
```

| Service | Owns | Talks to | Port |
| --- | --- | --- | --- |
| [gateway](gateway/README.md) | JWT validation, routing, WebSocket registry | every service, Kafka | 8080 |
| [auth-svc](services/auth-svc/README.md) | users, sessions, profile, avatars | Redis, MinIO, Kafka (outbox) | 8081 |
| [accounts-svc](services/accounts-svc/README.md) | account records, IBAN generation and resolve | ledger-svc, Kafka | 8082, gRPC 9082 |
| [ledger-svc](services/ledger-svc/README.md) | the append-only double-entry log, balance cache | Postgres only | gRPC 8083 |
| [transfers-svc](services/transfers-svc/README.md) | transfers, deposits, withdrawals, reconciliation | accounts, fraud, ledger, Stripe, Kafka | 8084 |
| [fraud-svc](services/fraud-svc/README.md) | three fixed rules and a log of every check | Postgres only | 8085, gRPC 9085 |
| [notifications-svc](services/notifications-svc/README.md) | transactional email, retry, dead-letter topic | Kafka, Mailpit | 8086 |

### One transfer, end to end

```mermaid
sequenceDiagram
    autonumber
    participant M as Maya
    participant G as Gateway
    participant T as transfers-svc
    participant A as accounts-svc
    participant F as fraud-svc
    participant L as ledger-svc
    participant K as Kafka
    participant R as Liam
    M->>G: POST /transfers/
    G->>T: + X-User-Id
    T->>A: resolve IBANs
    T->>T: INSERT pending
    T->>F: CheckTransfer
    F-->>T: approve
    T->>L: ExecuteTransfer
    Note over L: lock rows, check funds,<br/>2 entries, COMMIT
    L-->>T: tx id
    T->>T: completed + outbox
    T-->>M: 201 completed
    T--)K: TransferCompleted
    K--)G: transfer.events
    G--)R: balance.changed
    R->>G: GET /accounts/me
```

### Status lifecycles

```mermaid
stateDiagram-v2
    direction LR
    state "Transfer" as T {
        [*] --> pending
        pending --> rejected: a fraud rule fired
        pending --> completed: ledger posted both entries
        pending --> failed: ledger refused, e.g. insufficient funds
        pending --> completed: reconciliation found the entry
        pending --> failed: reconciliation found nothing
    }
    state "Deposit" as D {
        [*] --> d_pending
        d_pending: pending
        d_pending --> succeeded: Stripe webhook or poll
        d_pending --> d_failed: payment failed or abandoned
        d_failed: failed
        succeeded --> credited: worker posts to the ledger
        credited --> refunded: charge.refunded, reversal entry
    }
```

## Engineering highlights

Seven problems that a concurrency test, the failover test or the load test forced a decision on. Each links to the code.

| Problem | Decision | Code |
| --- | --- | --- |
| Two transfers on one account can both read "funds sufficient" before either writes | Lock both rows with `FOR UPDATE` in a fixed order before checking the balance | [`ledger.go:343`](services/ledger-svc/ledger.go#L343), [`ledger.go:312`](services/ledger-svc/ledger.go#L312) |
| Kafka and Stripe both deliver at least once, so a deposit or refund can arrive twice | Serialize posts per reference with `pg_advisory_xact_lock`, then check whether it already happened | [`ledger.go:530`](services/ledger-svc/ledger.go#L530) |
| A crash between "write the row" and "publish the event" loses or invents an event | Outbox in the same transaction; a relay publishes afterwards | [`outbox.go:84`](pkg/outbox/outbox.go#L84), [`relay.go:81`](pkg/outbox/relay.go#L81) |
| A gRPC call to the ledger can succeed while its response is lost | Keep the transfer `pending` and ask the ledger by reference | [`reconcile.go:58`](services/transfers-svc/reconcile.go#L58), [`reconcile.go:90`](services/transfers-svc/reconcile.go#L90) |
| Stripe and the ledger cannot share a transaction | Two statuses (`succeeded`, `credited`), a worker between them, refunds as reversal entries | [`deposit_reconcile.go:66`](services/transfers-svc/deposit_reconcile.go#L66), [`webhook.go:207`](services/transfers-svc/webhook.go#L207) |
| WebSocket messages can arrive late, reordered or not at all | Push a signal, never a value; the client re-reads over HTTP | [`notify.go:55`](gateway/notify.go#L55) |
| Naive failover can promote a replica that missed a confirmed commit | Patroni `synchronous_mode`: only the synchronous standby may be promoted | [`patroni.yml:99`](infra/patroni/patroni.yml#L99) |

Ten architecture decisions, each with the rejected alternative, are written up in [spec.md → Architecture decisions](spec.md#architecture-decisions-mini-adrs): a monorepo, a custom Gateway instead of Traefik or Kong, Kafka instead of NATS, an event instead of a synchronous call for account creation, the outbox, signal-only WebSocket messages, two deposit statuses, no NoSQL store, the profile living in auth-svc, and parallel requests on the profile screen.

## Observability

Every service exports OpenTelemetry spans to Jaeger. One `trace_id` covers the whole path of a request, and every SQL statement, `BEGIN`, `COMMIT` and `pool.acquire` is its own span. Credentials, emails, card data and account numbers never go into spans. The amount does, as a bare integer with no currency or account next to it, because a money trace without the amount cannot answer why a transfer was rejected.

<img src="docs/screenshots/jaeger-transfer-trace.png" alt="Jaeger trace gateway: POST /transfers/, 201 ms, 5 services, depth 7, 58 spans, with nested spans for gateway, transfers-svc and accounts-svc including pool.acquire and individual SQL queries." width="100%">

One completed transfer: 58 spans across five services in 201 ms.

<img src="docs/screenshots/jaeger-reconcile-trace.png" alt="Jaeger trace transfers-svc: reconcile transfer, 22 ms, 12 spans: GetTransactionByReference into ledger-svc, then UPDATE, INSERT and COMMIT in transfers-svc." width="100%">

The reconciliation worker's trace for the transfer that got stuck while fraud-svc was down. It is a separate trace, connected to the original request by a `FOLLOWS_FROM` span link rather than a parent-child relation, because the parent finished long before the worker started. The worker asked ledger-svc for the reference, found nothing, and closed the transfer as `failed`.

<img src="docs/screenshots/mailpit-inbox.png" alt="Mailpit inbox with Neo-Bank emails: transfer failed, transfer declined, and pairs of transfer sent and transfer received for 64.00, 18.75, 42.50 and 120.00 EUR, followed by verification emails." width="100%">

A successful transfer sends two emails, one to each side. A fraud block sends one email to the sender and does not name the rule. notifications-svc retries five times with backoff, then routes the event to `transfer.events.dlq` so one bad message cannot block the partition.

## Load test

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/charts/load-test-kpis-dark.svg">
  <img src="docs/charts/load-test-kpis-light.svg" alt="Load test headline: 176.4 transfers per second distributed ceiling; 31.5 transfers per second on one hot account at every concurrency level; 53,789 transfers and 87,888 ledger entries in the final run; 0 invariant violations." width="100%">
</picture>

k6 drives `POST /transfers/` through the Gateway, so every run measures the full path: one proxy hop, five gRPC calls, 24 SQL statements and four commits per transfer. The three profiles differ only in how each iteration picks sender and recipient. A Go tool, `loadtest/cmd/lt`, creates the fixtures through the public API, samples Postgres during the run and checks eight invariants afterwards.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/charts/load-throughput-latency-dark.svg">
  <img src="docs/charts/load-throughput-latency-light.svg" alt="Throughput and median latency against 10, 30, 60 and 120 virtual users. Distributed throughput 59.7, 151.0, 176.4, 171.9 transfers per second; hot account 31.6, 31.5, 31.5, 32.1. Median latency distributed 166, 188, 327, 683 ms; hot account 314, 938, 1893, 3778 ms." width="100%">
</picture>

The distributed profile peaks at 176.4 transfers/s at 60 virtual users. Past that point extra concurrency turns into queueing: from 60 to 120 users throughput stays flat while median latency doubles. The hot account holds 31.5 transfers/s from 10 to 120 users and its latency grows twelvefold, because every transfer into that account waits for the same row lock. That works out to 31.7 ms of lock hold time per transfer. The lock is the same one that keeps the account from going negative, so the ceiling is a chosen cost.

<details>
<summary>All twelve runs</summary>

40 accounts, a transfer of 100 minor units, 60 seconds per stage, latency measured as the full round trip through the Gateway.

| Profile | VUs | Requests/s | Executed/s | p50 | p95 | p99 | max | Errors |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| distributed | 10 | 59.7 | 59.7 | 166 ms | 208 ms | 237 ms | 386 ms | 0% |
| distributed | 30 | 151.0 | 151.0 | 188 ms | 280 ms | 353 ms | 523 ms | 0% |
| distributed | 60 | 176.4 | 176.4 | 327 ms | 464 ms | 546 ms | 760 ms | 0% |
| distributed | 120 | 171.9 | 171.9 | 683 ms | 847 ms | 942 ms | 1,070 ms | 0% |
| hot account | 10 | 31.6 | 31.6 | 314 ms | 405 ms | 490 ms | 657 ms | 0% |
| hot account | 30 | 31.5 | 31.5 | 938 ms | 1,136 ms | 1,477 ms | 2,137 ms | 0% |
| hot account | 60 | 31.5 | 31.5 | 1,893 ms | 2,180 ms | 2,386 ms | 2,962 ms | 0% |
| hot account | 120 | 32.1 | 32.1 | 3,778 ms | 4,016 ms | 4,312 ms | 4,988 ms | 0% |
| duplicates | 10 | 234.3 | 23.4 | 37 ms | 138 ms | 167 ms | 791 ms | 0% |
| duplicates | 30 | 507.4 | 50.8 | 43 ms | 203 ms | 260 ms | 435 ms | 0% |
| duplicates | 60 | 540.0 | 54.0 | 96 ms | 260 ms | 321 ms | 514 ms | 0% |
| duplicates | 120 | 560.0 | 56.0 | 200 ms | 363 ms | 434 ms | 589 ms | 0% |

Across all twelve runs there was no 5xx, no dropped connection, and no `failed`, `rejected` or unresolved transfer. The only outcomes were `completed` and, in the duplicates profile, `replayed`.

</details>

### Where the time goes

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/charts/slowest-transfer-trace-dark.svg">
  <img src="docs/charts/slowest-transfer-trace-light.svg" alt="Waterfall of the slowest transfer at 120 virtual users, 841 ms: ledger-svc spends 441 ms in pool.acquire waiting for a free connection and 247 ms waiting on the row lock." width="100%">
</picture>

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/charts/commit-cost-dark.svg">
  <img src="docs/charts/commit-cost-light.svg" alt="Stacked bar of a calm 126 ms transfer: four commits of 30.8, 18.4, 24.2 and 19.3 ms make 92.7 ms; everything else is 33.3 ms." width="100%">
</picture>

| # | Bottleneck | Measurement | Status |
| --- | --- | --- | --- |
| 1 | ledger-svc's connection pool: 16 connections, the `pgxpool` default of one per CPU, never configured | 441 of 841 ms in `pool.acquire`; `lock_waiters` caps at exactly 15 | Left as is; raising it means budgeting 7 pools against `max_connections = 200` |
| 2 | Four synchronously replicated commits per transfer | 92.7 of 126 ms on a calm request; sync flush lag peaks at 27.9 ms | Deliberate; it is what makes failover safe |
| 3 | The outbox relay publishes one message per `WriteMessages` call against a 1 s batch timeout | 1.0 event/s; the backlog grew from 1,140 to 34,216 rows during one profile | Known; the fix changes the relay's partial-failure semantics and needs its own tests |
| 4 | The overdraft check sums the account's whole journal while holding the lock | 0.019 ms at 1 entry, 1.6 ms at 10,003 entries | Known; switching to the balance cache moves the source of truth |

fraud-svc was a suspect and was cleared: its two velocity aggregates cost about 0.6 ms each on a covering index. The first thing to fall over was Jaeger, which held 13.3 of 15.5 GiB of memory after about 60,000 traced transfers at 100% sampling.

### Invariants checked after every profile

A load test that only reads HTTP status codes cannot tell a silent double-post from a clean 201, so `lt verify` runs eight SQL checks after each profile. All eight passed after all three profiles: 53,789 transfers, 87,888 entries, 0 violations.

| Check | Asserts |
| --- | --- |
| `entries_sum_zero` | `SUM(entries.amount) = 0` across the whole table |
| `no_negative_balances` | no test account went below zero |
| `transfer_entries_paired` | every completed transfer has exactly two entries with its reference: a debit, a credit, equal size |
| `balance_delta_matches_transfers` | per account, money moved according to `transfers` equals money moved in the ledger; two tables written by two services |
| `no_duplicate_idempotency_keys` | one `transfers` row per key |
| `balance_cache_matches_log` | `account_balances` equals `SUM(entries)` per account |
| `no_entries_for_failed_or_rejected` | no money moved for a failed or rejected transfer |
| `cohort_money_conserved` | the test accounts hold exactly what setup gave them |

The numbers describe this stack on one laptop: the load generator shares 16 CPUs with the system, Postgres runs on Docker Desktop for Windows where `fsync` is slow, and each stage ran once. Method, caveats and every bottleneck in detail: [spec.md → Load testing](spec.md#load-testing-k6--loadtest).

## Postgres failover

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/charts/failover-downtime-dark.svg">
  <img src="docs/charts/failover-downtime-light.svg" alt="Failover breakdown: about 20 s for the leader key TTL, about 2 s for election and promotion, 1 to 2 s for HAProxy. Four measured kills: 25.4, 23.6, 23.6 and 24.6 seconds of write downtime." width="100%">
</picture>

Three Postgres 16 nodes run under Patroni with etcd as the consensus store. HAProxy asks Patroni's REST API which node holds the leader key and routes port 5432 there. With `synchronous_mode`, the leader cannot confirm a commit until the synchronous standby has flushed it, and Patroni will promote only that standby. A confirmed transaction therefore always exists on the node that becomes the next leader.

`infra/failover/failover_test.go` kills the leader with `docker kill` while four goroutines send transfers through ledger-svc. It then checks that every confirmed transaction has both entries on the new leader, that `SUM(entries) = 0` per transaction, that no service container restarted, and that the old leader rejoins as a replica rather than a second leader. Most of the ~24 s is the leader key's 20 s TTL, which is Patroni's minimum.

The same kill, repeated by hand for this README on 9 October 2026, with a probe calling `GET /accounts/me` every 500 ms:

```text
+  0.0s  leader: pg-node1
+  0.4s  POST /transfers/        201 completed
+  2.7s  GET /accounts/me        200  (5 ms)
+  3.6s  docker kill neo-bank-pg-node1-1
+  3.6s  GET /accounts/me        500  (4 ms)
+ 28.1s  GET /accounts/me        200  (30 ms)       24.5 s without a leader
+ 28.1s  leader: pg-node3
+ 28.5s  POST /transfers/        201 completed
+ 29.5s  docker compose up -d pg-node1
+ 49.8s  pg-node1  replica       streaming
+ 49.8s  pg-node2  sync_standby  streaming
+ 49.8s  pg-node3  leader        running
```

No service was restarted. The connection pools reconnected on their own because HAProxy closes sessions to a node the moment it is marked down.

## Run locally

Requirements: Docker Desktop with Compose v2, Node.js 18 or later for the frontend, and Go 1.25 if you want to run tests or the dev tools outside containers.

```bash
git clone https://github.com/Adwerse/Neo-Bank && cd Neo-Bank
cp .env.example .env
docker compose up -d
cd frontend && npm install && npm run dev      # http://localhost:5173
```

The first build compiles the Go images and takes a few minutes. `docker compose ps` should show 17 running containers and two one-shot jobs, `kafka-init` and `minio-init`, that exit with code 0.

Everything except card deposits works without Stripe keys: registration, transfers, fraud checks, live updates and failover. To put money on an account without Stripe, use the dev tool. It moves money from a genesis account through the same `ExecuteTransfer` code path as a real transfer:

```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
LEDGER_GRPC_ADDR="localhost:8083" \
  go run ./services/ledger-svc/cmd/devtopup --account-id <id from GET /accounts/me> --amount 50000
```

| Variable | File | Purpose |
| --- | --- | --- |
| `STRIPE_SECRET_KEY` | `.env` | Stripe test-mode secret key (`sk_test_…`); transfers-svc refuses to start without a value |
| `STRIPE_WEBHOOK_SECRET` | `.env` | Webhook signing secret (`whsec_…`) printed by `stripe listen --forward-to localhost:8080/webhooks/stripe` |
| `RECONCILE_STALE_AFTER` | `.env` | Optional; how long a transfer may stay `pending` before the worker checks it. Default 2 minutes |
| `VITE_STRIPE_PUBLISHABLE_KEY` | `frontend/.env` | Stripe test-mode publishable key (`pk_test_…`) for the Payment Element |

Without a running `stripe listen`, deposits still settle: after two minutes the reconciliation worker polls Stripe for the PaymentIntent's status. Test cards are listed in [spec.md → Stripe test cards](spec.md#stripe-test-cards).

| URL | What it is |
| --- | --- |
| http://localhost:5173 | the web app (Vite dev server) |
| http://localhost:8080 | the Gateway, the only API entry point |
| http://localhost:8025 | Mailpit, every email the system sends |
| http://localhost:16686 | Jaeger, traces |
| `localhost:5432` / `5433` / `5434` | Postgres via HAProxy: current leader / any standby / synchronous standby |

[DEMO.md](DEMO.md) is a 5–10 minute walkthrough of the whole system, from registration to killing the Postgres leader, with a `curl` fallback for every step.

## Tests

```bash
# Go: unit and integration tests for every workspace module, against the running stack
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go list -m -f '{{.Dir}}' | while IFS= read -r dir; do (cd "$dir" && go test ./...); done

# Frontend: unit tests, lint, type check and production build
cd frontend && npm test && npm run lint && npm run build

# Failover: kills a Postgres container, so it only runs when asked
FAILOVER_TEST=1 go test ./infra/failover/... -v -count=1 -timeout 15m

# Load test: fixtures, relaxed velocity rules, three profiles, restore the rules
go run ./loadtest/cmd/lt setup -users 40 -fund 100000000
go run ./loadtest/cmd/lt fraud -mode loadtest
./loadtest/run.sh all
go run ./loadtest/cmd/lt fraud -mode restore
```

`go test ./...` from the repository root does not cover the modules, because the root has a `go.work` and no `go.mod`. The loop above is the same one CI uses.

## Repository layout

```text
gateway/            JWT, reverse proxy, WebSocket push, OpenAPI contract (openapi.yaml)
services/
  auth-svc/         users, sessions, profile, avatars in MinIO
  accounts-svc/     accounts, IBAN generation and resolve
  ledger-svc/       double-entry ledger; cmd/devtopup, cmd/seed
  transfers-svc/    transfers, deposits, withdrawals, Stripe webhook, reconciliation
  fraud-svc/        rule-based scoring
  notifications-svc/  email from Kafka events, retry and DLQ
pkg/                shared Go modules: outbox, pgha (pools and retry for failover), tracing, iban, health
proto/              gRPC contracts and generated Go code
infra/              Patroni, HAProxy and Postgres config; the failover test
loadtest/           k6 profiles and the lt tool (setup, probe, verify, report)
frontend/           React 19, TypeScript, Vite, TanStack Query, Recharts, Stripe Elements
docs/               screenshots and charts used in this README
```

## Limitations

These are choices, not unfinished work. The reasoning for each is in [spec.md → Honest limitations](spec.md#honest-limitations).

- **Withdrawals are simulated.** `POST /withdrawals` debits the balance through the ledger, but no payout leaves the system; a real payout needs a money transmitter licence.
- **Transfers stay inside the bank.** An IBAN from another bank is rejected with its own message. SEPA needs clearing-system membership.
- **The bank code is fictional.** IBANs pass the ISO 7064 check but use `ZZZZ`, so generated details never point at a real institution.
- **No recipient name on confirmation.** Registration collects no name, and a name lookup would turn IBAN resolve into an enumeration oracle.
- **No KYC.** Email verification proves mailbox ownership, not identity.
- **The refresh token is kept in `localStorage`.** An httpOnly cookie is the correct fix and needs a change to the token contract. The access token stays in memory.
- **Email is at-least-once.** An idempotency barrier makes duplicates rare; it cannot make them impossible.
- **Fraud checks fail closed.** While fraud-svc is down, transfers wait in `pending` instead of skipping the check.
- **A hot account tops out at 31.5 transfers/s.** That is the price of the row lock that prevents overdrafts.
- **Services trust each other.** Internal gRPC has no mTLS or shared secret.
- **Dev credentials live in `docker-compose.yml`.** Only the Stripe keys are real secrets, and they stay in the gitignored `.env`.
- **etcd is a single node.** Production needs three or five.
- **Tracing samples 100%.** Fine locally; at load-test volume Jaeger's in-memory store ran out of memory first.

## Further reading

- [spec.md](spec.md): the full engineering log, with every sprint, mini-ADR, manual verification and bottleneck write-up.
- [DEMO.md](DEMO.md): the live demo script, including how each step was verified.
- [DEVLOG.md](DEVLOG.md): the development diary.
- Each service directory has a short README covering what it owns, its API, and the events it publishes and consumes.

MIT licensed. Not a real bank.
