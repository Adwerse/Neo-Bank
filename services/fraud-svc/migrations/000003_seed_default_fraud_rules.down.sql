DELETE FROM fraud_rules WHERE rule_type IN ('amount_threshold', 'velocity_count', 'velocity_sum');
