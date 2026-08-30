# Demo script (5–10 minutes)

A sequence you can walk through live: from registration to killing the
primary Postgres. Each step covers what to show, what to say, and what
should happen. Every step has a `curl` fallback in case the UI hangs or
the browser misbehaves — a demo that breaks live is worse than no demo at
all, so it's better to know in advance what you'll fill the gap with.

The script is built around English/Latin data (email, amounts in EUR) —
there's nothing locale-specific in the UI.

## Setup (10–15 minutes before the demo)

1. `docker compose up -d`, wait until `docker compose ps` shows all
   containers as `Up`/`healthy` (see "Quick start" in the README).
2. `cd frontend && npm run dev` — frontend at http://localhost:5173.
3. If you're going to show a real deposit: real Stripe test keys in
   `.env` (`STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET` from `stripe
   listen`) — the placeholders from `.env.example` will bring the stack
   up, but a deposit won't go through with them. `docker compose up -d
   transfers-svc` after changing `.env`, and `stripe listen
   --forward-to localhost:8080/webhooks/stripe` in a separate terminal,
   left running. If you don't have keys — skip step 2 and show the
   deposit using the numbers from the README ("Load test" already proves
   that money moves for real; for this demo step you can settle for
   narration + code).
4. **Two separate browser profiles (or one regular + one incognito)** —
   not two tabs in the same profile. The refresh token lives in
   `localStorage`, which is shared across all tabs of the same origin:
   logging in as Bob in a second tab of the same profile will silently
   overwrite Alice's token. Two profiles are the only way to keep Alice
   and Bob logged in at the same time and show both updating without F5.
5. Open ahead of time and leave as background tabs: http://localhost:8025
   (Mailpit), http://localhost:16686 (Jaeger). Keep a terminal with
   `docker compose ps` handy for the last step.
6. Run through the whole script at least once before the demo — that's
   exactly how this file was written (see "How this was verified" at the
   end).

## 1. Registration → email code → login (~1.5 min)

**Show:** create a user Alice at http://localhost:5173/register (email +
password of 8+ characters). Switch to the Mailpit tab — the email with
the six-digit code arrives within seconds. Enter the code at
`/verify-email`, log in.

**Say:** the verification code proves ownership of the mailbox, not
identity; the project has no KYC, and that's a deliberate limitation (see
the README, "Honest limitations"). The email is genuinely sent over SMTP
— it's just that the receiver (`mailpit`) is local instead of a real
provider.

**Expected result:** after logging in — a dashboard with a balance of
`0.00 EUR`. Repeat for Bob in the second browser profile.

**Fallback (curl):**
```bash
curl -s -X POST http://localhost:8080/auth/register -H "Content-Type: application/json" \
  -d '{"email":"alice@example.com","password":"correct horse battery staple"}'
# the code is in Mailpit: http://localhost:8025/api/v1/search?query=to%3Aalice@example.com
curl -s -X POST http://localhost:8080/auth/verify-email -H "Content-Type: application/json" \
  -d '{"email":"alice@example.com","code":"<code from the email>"}'
curl -s -X POST http://localhost:8080/auth/login -H "Content-Type: application/json" \
  -d '{"email":"alice@example.com","password":"correct horse battery staple"}'
# {"access_token":"...", "refresh_token":"..."} — save the access_token
# into a variable (ALICE_TOKEN / BOB_TOKEN); all the fallback curl commands
# below use it: export ALICE_TOKEN="<access_token value>"
```

## 2. Deposit with a Stripe test card → balance goes up (~1.5 min)

**Show:** on Alice's dashboard — "Deposit", an amount (e.g. 500.00), the
card `4242 4242 4242 4242`, any future expiry, any CVC. After payment the
screen says "payment accepted, funds will be credited within a minute" —
**not** "balance topped up" right away. The balance updates itself,
without F5, once the background worker moves the deposit to `credited`
(usually seconds, not a minute).

**Say:** these are two separate facts at two separate times — Stripe
confirmed the card charge, the bank credited the money to the balance —
and the screen doesn't lie about that for a single second of the gap in
between. Full breakdown — README, "`succeeded` and `credited`" in the
mini-ADR section.

**Expected result:** Alice's balance is `500.00 EUR` without reloading
the page.

**Fallback:** if you don't have real Stripe keys, credit the money with
the dev tool instead (it doesn't go through Stripe, but runs through the
same `ledger-svc` code as any transfer — see the README, `cmd/devtopup`)
and narrate the `succeeded → credited` flow from the code:
```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
LEDGER_GRPC_ADDR="localhost:8083" \
  go run ./services/ledger-svc/cmd/devtopup --account-id <accounts.id from /accounts/me> --amount 50000
```

## 3. Transfer to a second user → both update without F5 (~1.5 min)

**Show:** on Alice's side — "Transfer", Bob's IBAN (visible on his
dashboard, `IE...`), an amount (e.g. 25.00). Send it. Switch to Bob's
profile — the balance and the activity feed have already updated
themselves (a WebSocket push → the frontend re-fetches `/accounts/me`).

**Say:** what travels over the WebSocket isn't the balance, it's a
"something changed for you" signal — the client itself re-fetches the
authoritative value over HTTP. The reason: an undelivered or reordered WS
push can never leave a stale number on screen, because there was never a
number in it to begin with (README, the "Signal, not data" mini-ADR).

**Expected result:** Alice's balance goes down, Bob's goes up — both
without pressing F5.

**Fallback:**
```bash
curl -s -X POST http://localhost:8080/transfers/ \
  -H "Authorization: Bearer $ALICE_TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: demo-transfer-1" \
  -d '{"recipient_iban":"<Bob's IBAN>","amount":2500}'
```

## 4. Transfer that trips a fraud rule → blocked with a reason (~1 min)

**Show:** a transfer from Alice for an amount over 5000.00 EUR (e.g.
6000.00). The response is a rejection with a stated reason, and the money
doesn't move.

**Say:** fraud-svc's decision is explainable: `triggered_rule` always
names exactly one rule
(`amount_threshold`/`velocity_count`/`velocity_sum`), never "some
combination." Bob gets no signal about this transfer whatsoever — the
recipient of a failed transfer isn't resolved even at the level of
looking up a `user_id` (README, "Security: the recipient of a failed
transfer isn't resolved at all").

**Expected result:** status `rejected`, `failure_reason:
"amount_threshold"`, Alice's balance unchanged.

**Fallback:**
```bash
curl -s -X POST http://localhost:8080/transfers/ \
  -H "Authorization: Bearer $ALICE_TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: demo-fraud-1" \
  -d '{"recipient_iban":"<Bob's IBAN>","amount":600000}'
# {"status":"rejected","failure_reason":"amount_threshold"}
```

## 5. Jaeger: tracing one transfer across 4 services (~1.5 min)

**Show:** http://localhost:16686 → Service = `gateway`, Operation =
`POST /transfers/` → Find Traces → open the most recent one (the transfer
from step 3, or a new one). Expand the span tree.

**Say:** a single `trace_id` covers the whole path: Gateway →
transfers-svc → accounts-svc/fraud-svc/ledger-svc, with every SQL query
and `BEGIN`/`COMMIT` as its own span — that's literally the tracing
sprint's definition of done. If a slower-than-usual trace happens to be
at hand, you can point straight at `pool.acquire` as its own span and
talk through bottleneck #1 from the load test (the `ledger-svc`
connection pool).

**Expected result:** a span tree with the names of every service
involved, with the duration of each hop visible at a glance.

## 6. Trace of a stuck transfer resolved by reconciliation (~1.5–2 min)

**Show:**
```bash
docker compose stop fraud-svc
curl -s -X POST http://localhost:8080/transfers/ \
  -H "Authorization: Bearer $ALICE_TOKEN" -H "Content-Type: application/json" \
  -H "Idempotency-Key: demo-stuck-1" \
  -d '{"recipient_iban":"<Bob's IBAN>","amount":1000}'
# 202 {"status":"pending","message":"fraud check unavailable, transfer still pending"}
docker compose start fraud-svc
```
Wait ~20–30 seconds (`RECONCILE_STALE_AFTER=20s` in `.env` — deliberately
shortened for the demo; the production value is 2 minutes), refresh
Alice's activity feed: the transfer has moved out of `pending` on its own
— here into `failed`/`timeout_unresolved`, **and that's the correct
outcome**, not a demo malfunction.

**Say (important not to confuse this with "the money just arrived
late"):** the worker doesn't guess — it asks
`ledger-svc.GetTransactionByReference` directly what actually happened,
and reports it honestly, based on the facts. In this scenario fraud-svc
was unavailable BEFORE the call to `ledger-svc` (the transfer got stuck
at the fraud check), so `ledger-svc` never saw this transaction at all —
reconciliation finds "no record" and closes the transfer as `failed`,
rather than inventing a success. The opposite case — the ledger actually
posted the money, but the response never made it back to
`transfers-svc` (the connection dropped after the call, a genuine saga
problem) — resolves to `completed`; that case is also real and
documented (README, "How the crash was simulated for testing"), but it
only reproduces through the debug flag
`SIMULATE_CRASH_AFTER_LEDGER_CALL`, which isn't worth enabling during a
live demo — the risk of breaking the demo outweighs the payoff of the
flashier version. The key point is the same in both cases: a transfer
never stays `pending` forever, because the worker checks with
`ledger-svc` instead of staying silent.

In Jaeger, this transfer has **two** traces: the original request and a
separate worker trace later, linked via a span link rather than a
parent-child relationship (README, "Span links: a link, not a parent" —
a parent can't finish before its child, and here there's a real time gap
between them).

**Expected result:** transfer status `failed`, `failure_reason:
"timeout_unresolved"`, Alice's balance untouched (the money never
moved); in Jaeger — a second trace with a `span link` to the first,
attribute `reconcile.trigger=stale_pending`.

**Advanced variant (optional, requires a separate rehearsal ahead of
time, not for improvising during the show):** to demonstrate the
`completed` outcome specifically ("the ledger posted the money, the
response got lost"), you need to bring up `transfers-svc` with
`SIMULATE_CRASH_AFTER_LEDGER_CALL=true` and a shortened
`RECONCILE_STALE_AFTER`, make a transfer (the client gets a `502`, the
container crashes), then restart `transfers-svc` without the variable —
the worker will bring the transfer to `completed` on its next tick. The
procedure and its output — README, "How the crash was simulated for
testing".

## 7. Kill the primary Postgres → the system survives failover (~2 min)

**Show:**
```bash
# who's the current leader
docker compose exec -T pg-node1 curl -s http://127.0.0.1:8008/cluster
# kill its container by name (e.g. pg-node2, if that's the current leader)
docker kill neo-bank-pg-node2-1
```
Right after — a transfer in the UI or via curl fails/hangs for a few
seconds. **While you wait, deliver the rehearsed number:** in the runs
done for the README, write downtime took 23.6–25.4 seconds — that's
mostly (~20 s) the leader key's TTL in etcd, a hard Patroni minimum, not
something you can tune away in config. Then retry the transfer — it goes
through.

**Say:** not a single confirmed transaction is lost — that's exactly
what `infra/failover/failover_test.go` verifies on every run:
`SUM(entries) = 0` before and after, both rows of every ledger entry
present on the new leader, and the old leader comes back as a
**replica** after restart, not as a second leader (split-brain). The
downtime is long not because something is poorly configured — Patroni's
`synchronous_mode` guarantees that a confirmed transaction physically
sits on the one node eligible to become leader; that guarantee is
exactly what costs ~24 seconds.

**Expected result:** a transfer during the downtime — error/timeout; a
transfer after ~25 seconds — `completed` again; `docker compose ps` shows
the killed node back and `Up`.

**Recovery after the demo:** the crashed node comes back up and joins as
a replica on its own — nothing needs to be touched, `docker compose ps`
will show it `Up (healthy)` within ~30–40 seconds.

## If something goes wrong

- **Step 2 (deposit) doesn't go through** — almost always means
  placeholder Stripe keys in `.env` instead of real test ones, or
  `stripe listen` isn't running. The prepared fallback — `devtopup` —
  makes the same `succeeded → credited` point without real Stripe.
- **Step 3/6 "didn't update without F5"** — almost always two tabs in
  the same browser profile (see "Setup", item 4): the most recent login
  overwrites the previous user's refresh token in `localStorage`. Log
  the "dropped" user back in, in their own profile.
- **Step 6 doesn't resolve within 30 seconds** — check `docker compose
  logs transfers-svc | grep reconcil`; the worker ticks once every
  `RECONCILE_STALE_AFTER`, so worst case that's almost double the time.
  Don't panic before 45 seconds.
- **Step 7 "stuck for good"** — if more than ~40 seconds have passed
  with no recovery, check `docker compose exec pg-node1 curl -s
  http://127.0.0.1:8008/cluster` on a different node: if no leader got
  elected, it's almost certainly etcd itself (a single node in the dev
  topology — README, "Honest limitations") that's unavailable. This is
  exactly the documented single SPOF of the topology; `docker compose
  restart etcd` and try again.

## How this was verified

Steps 1–4 and 6 were run end-to-end via `curl` against a live `docker
compose up -d` stack (not just read out of the code): register → code
from Mailpit → verify → login → account provisioning via the async
pipeline → an ordinary transfer (balance on both sides changes by
exactly the transfer amount) → a transfer for 6000.00 EUR
(`rejected`/`amount_threshold`, balance untouched) → stop fraud-svc →
transfer (`202 pending`) → start fraud-svc → about ~30 seconds later the
transfer turned into `failed`/`timeout_unresolved` on its own. It was
precisely this last result that forced a rewrite of step 6: the first
draft of this file claimed the transfer would resolve to `completed` —
which is false for the "fraud-svc unavailable" scenario (ledger-svc
never saw this transaction, so reconciliation has nothing to find), and
only a live run revealed that; the code and the devlog confirm it
(`resolution=failed` in the write-up of this same scenario). It's left
in as a lesson in the file itself rather than silently fixed — exactly
what the introduction warns about: "a demo that breaks live is worse
than no demo at all," and an unnoticed inaccuracy in the script is the
same thing, just before the show instead of during it.

The UI portion (clicks, "without F5," Jaeger and Mailpit in browser
tabs) isn't automated — this environment has no browser tool that could
verify it live; steps 1–4 and 6 describe exactly the same API that was
just verified with curl, one to one.

Step 7 was also run for real on this stack, not just read out of the
code: `docker compose exec pg-node1 curl .../cluster` (leader —
`pg-node3`) → `docker kill neo-bank-pg-node3-1` → a series of
`POST /transfers/` immediately started answering `500` → the cluster
elected a new leader (`pg-node1`, a new `timeline` in the `/cluster`
output) → `docker compose up -d pg-node3` → the transfer was `completed`
again on the very first request. The stopwatch in this run isn't
precise (the probe script polled more often than once a second, and its
own log isn't a reliable source for the downtime figure) — the
recovery-without-intervention itself is confirmed live, but the precise
downtime figure for the demo should be taken from the README ("Measured
failover time," three runs of the strict `failover_test.go`: 23.6–25.4
s) — that's more honest than the stopwatch figure from this session.
