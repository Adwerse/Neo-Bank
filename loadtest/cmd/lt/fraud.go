package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5"
)

// loadTestThresholds are what `lt fraud -mode loadtest` raises the velocity
// rules to.
//
// The point of raising them, and the reason only threshold_value moves:
// fraud-svc's production defaults are velocity_count > 5 per 300s and
// velocity_sum > 1_000_000 per 3600s (migration 000003). Any sustained load
// blows through five transfers per sender in the first second, after which
// every subsequent request is rejected at the fraud step and never reaches
// ledger-svc at all. The run would then measure how fast the system can
// reject things — a real number, but not the one this exercise is about.
//
// What is deliberately NOT changed: `enabled` stays true and
// `window_seconds` stays exactly as it was. Both velocity rules therefore
// run the SAME two aggregate queries over fraud_checks on every single
// transfer, with the same window and the same growing table underneath
// them. The cost of the rule — which is the thing being measured, since
// fraud-svc is a candidate bottleneck precisely because it queries history
// per request — is untouched. Only the comparison at the end flips from
// "reject" to "approve".
//
// amount_threshold is left alone too: the k6 scripts send amounts far below
// it, so it never trips and needs no help.
var loadTestThresholds = map[string]int64{
	"velocity_count": 1_000_000_000,
	"velocity_sum":   1_000_000_000_000_000,
}

type fraudRuleBackup struct {
	RuleType       string `json:"rule_type"`
	ThresholdValue int64  `json:"threshold_value"`
	WindowSeconds  *int32 `json:"window_seconds"`
	Enabled        bool   `json:"enabled"`
}

func runFraud(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("fraud", flag.ExitOnError)
	mode := fs.String("mode", "show", "show | loadtest | restore")
	databaseURL := fs.String("database-url", envOr("DATABASE_URL", defaultDatabaseURL), "Postgres URL of the current leader")
	backupPath := fs.String("backup", "loadtest/fixtures/fraud-rules.backup.json", "where the pre-run rule values are saved and restored from")
	if err := fs.Parse(args); err != nil {
		return err
	}

	conn, err := pgx.Connect(ctx, *databaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer conn.Close(ctx)

	switch *mode {
	case "show":
		rules, err := readFraudRules(ctx, conn)
		if err != nil {
			return err
		}
		for _, r := range rules {
			window := "null"
			if r.WindowSeconds != nil {
				window = fmt.Sprintf("%ds", *r.WindowSeconds)
			}
			fmt.Printf("%-18s threshold=%-18d window=%-8s enabled=%t\n", r.RuleType, r.ThresholdValue, window, r.Enabled)
		}
		return nil

	case "loadtest":
		rules, err := readFraudRules(ctx, conn)
		if err != nil {
			return err
		}
		// Only back up if there is no backup yet. Re-running `-mode
		// loadtest` between profiles must not overwrite the real values
		// with the already-raised ones — that would make restore a no-op
		// and quietly leave the dev stack with fraud detection disabled.
		if _, err := os.Stat(*backupPath); os.IsNotExist(err) {
			if err := writeFraudBackup(*backupPath, rules); err != nil {
				return err
			}
			log.Printf("fraud: saved original rule values to %s", *backupPath)
		} else {
			log.Printf("fraud: %s already exists, keeping it as the restore point", *backupPath)
		}
		for ruleType, threshold := range loadTestThresholds {
			tag, err := conn.Exec(ctx,
				"UPDATE fraud_rules SET threshold_value = $1 WHERE rule_type = $2",
				threshold, ruleType,
			)
			if err != nil {
				return fmt.Errorf("raise %s: %w", ruleType, err)
			}
			if tag.RowsAffected() == 0 {
				return fmt.Errorf("raise %s: no such rule in fraud_rules", ruleType)
			}
			log.Printf("fraud: %s threshold -> %d (window and enabled unchanged)", ruleType, threshold)
		}
		return nil

	case "restore":
		data, err := os.ReadFile(*backupPath)
		if err != nil {
			return fmt.Errorf("read backup (nothing to restore?): %w", err)
		}
		var rules []fraudRuleBackup
		if err := json.Unmarshal(data, &rules); err != nil {
			return fmt.Errorf("parse backup: %w", err)
		}
		for _, r := range rules {
			if _, err := conn.Exec(ctx,
				"UPDATE fraud_rules SET threshold_value = $1, window_seconds = $2, enabled = $3 WHERE rule_type = $4",
				r.ThresholdValue, r.WindowSeconds, r.Enabled, r.RuleType,
			); err != nil {
				return fmt.Errorf("restore %s: %w", r.RuleType, err)
			}
		}
		if err := os.Remove(*backupPath); err != nil {
			return fmt.Errorf("remove backup after restore: %w", err)
		}
		log.Printf("fraud: restored %d rules from backup", len(rules))
		return nil

	default:
		return fmt.Errorf("unknown -mode %q (want show, loadtest or restore)", *mode)
	}
}

func readFraudRules(ctx context.Context, conn *pgx.Conn) ([]fraudRuleBackup, error) {
	rows, err := conn.Query(ctx, "SELECT rule_type, threshold_value, window_seconds, enabled FROM fraud_rules ORDER BY rule_type")
	if err != nil {
		return nil, fmt.Errorf("read fraud_rules: %w", err)
	}
	defer rows.Close()

	var rules []fraudRuleBackup
	for rows.Next() {
		var r fraudRuleBackup
		if err := rows.Scan(&r.RuleType, &r.ThresholdValue, &r.WindowSeconds, &r.Enabled); err != nil {
			return nil, fmt.Errorf("scan fraud_rules: %w", err)
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

func writeFraudBackup(path string, rules []fraudRuleBackup) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
