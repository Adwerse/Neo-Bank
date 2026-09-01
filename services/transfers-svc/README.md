# transfers-svc

The busiest service: every transfer, deposit, and withdrawal passes through
here, and it owns the sagas that keep them honest when a downstream call's
outcome is unknown.

**Owns:** `transfers`, `deposits`, `withdrawals`; the reconciliation workers
that resolve stuck `pending` transfers and `succeeded`-but-not-`credited`
deposits; the Stripe webhook handler and its signature verification.

**API:** `POST /transfers/`, `GET /transfers` (the unified operation-history
feed — transfers, deposits, withdrawals merged), `POST /deposits`,
`GET /deposits/{id}`, `POST /withdrawals` (simulated payout, see the root
README's "Honest limitations"), `POST /webhooks/stripe`.

**Publishes:** `TransferCompleted`/`Failed`/`Rejected` and `DepositCredited`
to `transfer.events`, through the same outbox pattern as auth-svc.

**Talks to:** accounts-svc (resolve the recipient), fraud-svc
(`CheckTransfer`, before ledger-svc is ever called), ledger-svc
(`ExecuteTransfer`/`Deposit`/`ReverseDeposit`), Stripe (REST + webhook), its
own Postgres schema, Kafka.

Full reasoning — the fraud-then-ledger ordering, `pending` as an honest
"don't know" state, the Stripe `succeeded`→`credited` saga: root
[README](../../README.md), "Engineering highlights".
