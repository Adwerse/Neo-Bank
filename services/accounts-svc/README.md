# accounts-svc

Owns the account record and the one question every transfer starts with:
does an account with this IBAN exist.

**Owns:** `accounts` (account number, IBAN, status), IBAN generation and
check-digit validation (`pkg/iban`), and the rate-limited IBAN resolve path
that keeps `ResolveAccountByIban` from being an enumeration oracle.

**API:** `GET /accounts/me` (balance included — fetched live from ledger-svc,
never cached in this service's own table); internally, gRPC
`ResolveAccountByIban`, `GetAccountByUserID`, `GetBalance`-adjacent calls for
the Gateway's WS bridge.

**Consumes:** `user.events` (`UserActivated` → create the account row, then
call `ledger-svc.CreateLedgerAccount`). **Publishes:** `account.events`
(`AccountCreated`) so `notifications-svc` and the Gateway's account cache can
build their own projections without calling back here.

**Talks to:** ledger-svc (gRPC, for the balance and account creation), its
own Postgres schema, Kafka.

Full reasoning — three-way IBAN resolve failure codes, why recipient names
are never shown: root [README](../../README.md).
