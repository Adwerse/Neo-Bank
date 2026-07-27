CREATE TABLE fraud_checks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    transfer_id UUID NOT NULL,
    account_id UUID NOT NULL,
    amount BIGINT NOT NULL,
    decision TEXT NOT NULL CHECK (decision IN ('approve', 'reject')),
    triggered_rule TEXT,
    details JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_fraud_checks_account_id_created_at ON fraud_checks (account_id, created_at);
