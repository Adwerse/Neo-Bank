CREATE TABLE entries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transaction_id UUID NOT NULL,
    ledger_account_id UUID NOT NULL REFERENCES ledger_accounts(id),
    amount BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_entries_ledger_account_id ON entries (ledger_account_id);
CREATE INDEX idx_entries_transaction_id ON entries (transaction_id);
