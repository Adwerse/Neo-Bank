-- Deterministically provisions the system's genesis ledger account: the
-- counterparty every issuance of new money (Stripe-funded deposits via the
-- Deposit RPC, and historically cmd/seed's/cmd/devtopup's own direct
-- mints) is posted against. Both id and account_id are pinned to the same
-- fixed UUID so the row is fully deterministic across environments, not
-- merely unique — this is the same constant already hardcoded
-- independently as genesisAccountID in cmd/seed/main.go and
-- cmd/devtopup/main.go; this migration is what actually guarantees the row
-- exists on a fresh database before either tool, or the Deposit RPC, ever
-- runs. Their own idempotent upserts remain harmless no-ops once this
-- migration has already created the row.
INSERT INTO ledger_accounts (id, account_id)
VALUES ('00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000001')
ON CONFLICT (account_id) DO NOTHING;
