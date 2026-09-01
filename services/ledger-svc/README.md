# ledger-svc

The money. An append-only, double-entry log and nothing else — no HTTP API,
no Kafka, not reachable from the Gateway at all.

**Owns:** `entries` (the append-only double-entry log, never updated or
deleted) and `account_balances` (an incrementally-maintained cache over that
log, always reconstructible from it). The invariant this service exists to
hold: `SUM(entries.amount) = 0`, globally and per transaction, even under
concurrent writes to the same account.

**API:** internal gRPC only (`ledger.v1.LedgerService`) —
`GetBalance`, `ExecuteTransfer`, `Deposit`, `ReverseDeposit`, `GetHistory`,
`GetTransactionByReference`. Its only caller is transfers-svc; accounts-svc
calls it once, to create a new account's ledger row.

**Talks to:** its own Postgres schema. No other service, no Kafka, no
external system.

Concurrency-safe transfers, the genesis-account emission model, and the
reference-based idempotency on `Deposit`/`ReverseDeposit`: root
[README](../../README.md), "Engineering highlights".
