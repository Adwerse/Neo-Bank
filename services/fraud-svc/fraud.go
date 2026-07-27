package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ruleEvaluationOrder is fixed and deliberate: rules are checked in this
// order, and the first one that trips ends evaluation immediately — later
// rules are never computed for that call. This is what makes a reject
// explainable: triggered_rule always names exactly one rule, never "some
// combination of them".
var ruleEvaluationOrder = []string{"amount_threshold", "velocity_count", "velocity_sum"}

type ruleConfig struct {
	ThresholdValue int64
	WindowSeconds  *int32
}

// checkTransfer scores one transfer against the enabled rules in
// fraud_rules and records the outcome in fraud_checks, both inside a single
// transaction: either a decision is computed AND logged, or neither
// happens. A DB failure at any point returns an error rather than an
// approve — fraud-svc fails closed, it never silently lets a transfer
// through because it couldn't compute an answer. What to do about an
// unavailable fraud-svc is the caller's decision, not this function's.
func checkTransfer(ctx context.Context, pool *pgxpool.Pool, transferID, accountID string, amount int64) (decision string, triggeredRule string, reason string, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", "", "", fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	rules, err := loadEnabledRules(ctx, tx)
	if err != nil {
		return "", "", "", fmt.Errorf("load fraud rules: %w", err)
	}

	details := make(map[string]any, len(ruleEvaluationOrder))

	for _, ruleType := range ruleEvaluationOrder {
		rule, enabled := rules[ruleType]
		if !enabled {
			continue
		}

		var observed int64
		switch ruleType {
		case "amount_threshold":
			observed = amount

		case "velocity_count":
			if rule.WindowSeconds == nil {
				return "", "", "", fmt.Errorf("rule %s: window_seconds is required but NULL", ruleType)
			}
			priorCount, err := countRecentApprovedTransfers(ctx, tx, accountID, *rule.WindowSeconds)
			if err != nil {
				return "", "", "", fmt.Errorf("count recent approved transfers: %w", err)
			}
			// +1: "if this transfer were also approved, would the count in
			// the window exceed the limit" — not just the count of
			// transfers that already happened.
			observed = priorCount + 1

		case "velocity_sum":
			if rule.WindowSeconds == nil {
				return "", "", "", fmt.Errorf("rule %s: window_seconds is required but NULL", ruleType)
			}
			priorSum, err := sumRecentApprovedTransfers(ctx, tx, accountID, *rule.WindowSeconds)
			if err != nil {
				return "", "", "", fmt.Errorf("sum recent approved transfers: %w", err)
			}
			observed = priorSum + amount
		}

		details[ruleType] = map[string]any{
			"observed":       observed,
			"threshold":      rule.ThresholdValue,
			"window_seconds": rule.WindowSeconds,
		}

		if observed > rule.ThresholdValue {
			triggeredRule = ruleType
			reason = fmt.Sprintf("%s: observed %d exceeds threshold %d", ruleType, observed, rule.ThresholdValue)
			break
		}
	}

	if triggeredRule == "" {
		decision = "approve"
		reason = "no rule triggered"
	} else {
		decision = "reject"
	}

	detailsJSON, err := json.Marshal(details)
	if err != nil {
		return "", "", "", fmt.Errorf("marshal details: %w", err)
	}

	if err := recordFraudCheck(ctx, tx, transferID, accountID, amount, decision, triggeredRule, detailsJSON); err != nil {
		return "", "", "", fmt.Errorf("record fraud check: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", "", "", fmt.Errorf("commit transaction: %w", err)
	}

	return decision, triggeredRule, reason, nil
}

func loadEnabledRules(ctx context.Context, tx pgx.Tx) (map[string]ruleConfig, error) {
	rows, err := tx.Query(ctx, "SELECT rule_type, threshold_value, window_seconds FROM fraud_rules WHERE enabled = true")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	rules := make(map[string]ruleConfig)
	for rows.Next() {
		var ruleType string
		var cfg ruleConfig
		if err := rows.Scan(&ruleType, &cfg.ThresholdValue, &cfg.WindowSeconds); err != nil {
			return nil, err
		}
		rules[ruleType] = cfg
	}
	return rules, rows.Err()
}

// countRecentApprovedTransfers counts this account's own approved checks in
// the last windowSeconds — fraud-svc scores velocity off its own history
// (fraud_checks), never by querying transfers-svc's database directly.
func countRecentApprovedTransfers(ctx context.Context, tx pgx.Tx, accountID string, windowSeconds int32) (int64, error) {
	var count int64
	err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM fraud_checks
		 WHERE account_id = $1 AND decision = 'approve' AND created_at >= now() - make_interval(secs => $2)`,
		accountID, windowSeconds,
	).Scan(&count)
	return count, err
}

func sumRecentApprovedTransfers(ctx context.Context, tx pgx.Tx, accountID string, windowSeconds int32) (int64, error) {
	var sum int64
	err := tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount), 0) FROM fraud_checks
		 WHERE account_id = $1 AND decision = 'approve' AND created_at >= now() - make_interval(secs => $2)`,
		accountID, windowSeconds,
	).Scan(&sum)
	return sum, err
}

func recordFraudCheck(ctx context.Context, tx pgx.Tx, transferID, accountID string, amount int64, decision, triggeredRule string, detailsJSON []byte) error {
	var triggeredRuleParam any
	if triggeredRule != "" {
		triggeredRuleParam = triggeredRule
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO fraud_checks (transfer_id, account_id, amount, decision, triggered_rule, details)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb)`,
		transferID, accountID, amount, decision, triggeredRuleParam, string(detailsJSON),
	)
	return err
}
