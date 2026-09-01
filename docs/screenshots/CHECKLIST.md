# Screenshot/gif checklist

Not filled in automatically — the environment this commit was prepared in
had no browser tool for real UI/Jaeger screenshots, and it isn't worth
inserting broken image links or passing off a mockup as a real screen.
Below is the exact list of what to capture, with the [DEMO.md](../../DEMO.md)
step each screenshot reproduces, and the expected filename. Capture each
one by following DEMO.md's steps (stack up, `npm run dev` in `frontend/`),
drop it here next to this file, and uncomment the corresponding line in
the README (see "Screenshots" right at the top, next to "Quick start") —
`![caption](docs/screenshots/<file>)`.

| file | DEMO.md step | what's on screen |
|---|---|---|
| `01-dashboard-empty.png` | 1 | Dashboard right after login: balance `0.00 EUR`, empty operations feed |
| `02-mailpit-code.png` | 1 | Mailpit UI with the email open, six-digit code visible |
| `03-deposit-pending.png` | 2 | Deposit screen right after `confirmPayment`: "payment accepted, crediting within a minute" — **before** the balance updates |
| `04-deposit-credited.png` | 2 | Same dashboard seconds later: balance already grew, no reload |
| `05-transfer-both-sides.gif` | 3 | gif or two screenshots side by side: Alice's and Bob's dashboards before and right after the transfer, both updated without F5 (two different browser profiles — see DEMO.md, "Preparation") |
| `06-fraud-blocked.png` | 4 | Response/toast with the rejection and reason (`amount_threshold`) on the transfer form |
| `07-jaeger-transfer-trace.png` | 5 | Jaeger, expanded span tree for one transfer — gateway → transfers-svc → accounts-svc/fraud-svc/ledger-svc visible |
| `08-jaeger-reconciliation-link.png` | 6 | Jaeger, the `outbox publish` span or the reconciliation worker's span with a span link back to the original trace |
| `09-failover-terminal.png` | 7 | Terminal: `docker kill`, a run of failed transfer attempts, then a successful one — this can just be pasted as text into the README instead of an image, see the example below |

Step 7 doesn't have to be a screenshot — a real terminal log is more
honest than a screenshot and easier to keep up to date, e.g.
`docker kill neo-bank-pg-node3-1` followed by a series of transfer
attempts up to the first success. The exact downtime numbers (three runs
of the strict automated test, not a manual curl script) are already in
[spec.md](../../spec.md), "Postgres: replication and automatic failover" →
"Measured failover time" (23.6–25.4 s); trust those numbers for the
screenshot/log demo, not a stopwatch during the presentation.
