CREATE TABLE fraud_rules (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    rule_type TEXT NOT NULL UNIQUE CHECK (rule_type IN ('amount_threshold', 'velocity_count', 'velocity_sum')),
    enabled BOOLEAN NOT NULL DEFAULT true,
    threshold_value BIGINT NOT NULL,
    window_seconds INT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
