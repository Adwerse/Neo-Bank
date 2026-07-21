CREATE TABLE account_balances (
    ledger_account_id UUID PRIMARY KEY REFERENCES ledger_accounts(id),
    balance BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Backfill one row per existing ledger_accounts, computed from the entries
-- log (the source of truth) at migration time. The LEFT JOIN + COALESCE
-- covers accounts with zero entries too, giving every ledger account a
-- balance row from the moment this table exists.
INSERT INTO account_balances (ledger_account_id, balance, updated_at)
SELECT la.id, COALESCE(SUM(e.amount), 0), now()
FROM ledger_accounts la
LEFT JOIN entries e ON e.ledger_account_id = la.id
GROUP BY la.id;
