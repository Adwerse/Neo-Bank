# fraud-svc

Rule-based transfer scoring — three fixed rules, checked in order, first
match wins. No ML, deliberately: every decision has to be explainable by
naming the one rule that fired.

**Owns:** `fraud_rules` (configuration — `amount_threshold`,
`velocity_count`, `velocity_sum`) and `fraud_checks`, an append-only log of
every check made, both `approve` and `reject` — the log is what makes the
velocity rules computable at all, and it doubles as the audit trail for
"why was my transfer blocked."

**API:** internal gRPC only (`fraud.v1.FraudService.CheckTransfer`), called
by transfers-svc after a transfer's `pending` row exists but before
ledger-svc is ever touched.

**Talks to:** its own Postgres schema. No other service, no Kafka. Fails
closed: any error here (unavailable, an internal error) means the calling
transfer stays `pending`, never silently approved.

Rule definitions, the fail-closed reasoning, and the load-test result that
ruled fraud-svc *out* as a bottleneck: root [README](../../README.md).
