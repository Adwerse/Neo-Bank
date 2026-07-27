INSERT INTO fraud_rules (rule_type, threshold_value, window_seconds) VALUES
    ('amount_threshold', 500000, NULL),
    ('velocity_count', 5, 300),
    ('velocity_sum', 1000000, 3600);
