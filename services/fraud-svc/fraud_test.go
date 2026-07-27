package main

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// newTestPool connects to the postgres instance from docker-compose.yml via
// DATABASE_URL, skipping the test if it isn't set — these tests exercise
// real SQL (velocity windows, JSONB), not something worth mocking out.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set; skipping test that requires a live postgres (see docker-compose.yml)")
	}

	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// randomUUID mints a UUID v4 by hand, matching ledger-svc/transfers-svc's own
// randomUUID convention — this repo hand-rolls UUIDs in Go rather than add
// github.com/google/uuid as a dependency. fraud_checks.account_id/transfer_id
// have no foreign keys, so tests never need a real account or transfer row.
func randomUUID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		t.Fatalf("generate random uuid: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx (RFC 4122)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// cleanupFraudChecks deletes every fraud_checks row for accountID once the
// test ends. Each test uses its own fresh random account_id, so this is
// purely hygiene, not a correctness requirement.
func cleanupFraudChecks(t *testing.T, pool *pgxpool.Pool, accountID string) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM fraud_checks WHERE account_id = $1", accountID); err != nil {
			t.Logf("cleanup: delete fraud_checks for account_id=%s: %v", accountID, err)
		}
	})
}

// fetchFraudCheck reads back the persisted row for transferID, proving
// checkTransfer actually logged the decision it returned.
func fetchFraudCheck(t *testing.T, ctx context.Context, pool *pgxpool.Pool, transferID string) (decision string, triggeredRule *string, details []byte) {
	t.Helper()
	err := pool.QueryRow(ctx,
		"SELECT decision, triggered_rule, details FROM fraud_checks WHERE transfer_id = $1",
		transferID,
	).Scan(&decision, &triggeredRule, &details)
	if err != nil {
		t.Fatalf("fetch fraud_checks row for transfer_id=%s: %v", transferID, err)
	}
	return decision, triggeredRule, details
}

// Seeded defaults from migrations/000003_seed_default_fraud_rules.up.sql:
// amount_threshold = 500000, velocity_count = 5 per 300s, velocity_sum =
// 1000000 per 3600s. Tests rely on these rather than inserting their own
// rule rows, since fraud_rules has exactly one row per rule_type (UNIQUE).

func TestCheckTransfer_AmountThreshold_Reject(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	accountID := randomUUID(t)
	transferID := randomUUID(t)
	cleanupFraudChecks(t, pool, accountID)

	decision, triggeredRule, _, err := checkTransfer(ctx, pool, transferID, accountID, 600000)
	if err != nil {
		t.Fatalf("checkTransfer: %v", err)
	}
	if decision != "reject" {
		t.Errorf("decision = %q, want reject", decision)
	}
	if triggeredRule != "amount_threshold" {
		t.Errorf("triggeredRule = %q, want amount_threshold", triggeredRule)
	}

	gotDecision, gotTriggered, _ := fetchFraudCheck(t, ctx, pool, transferID)
	if gotDecision != "reject" {
		t.Errorf("persisted decision = %q, want reject", gotDecision)
	}
	if gotTriggered == nil || *gotTriggered != "amount_threshold" {
		t.Errorf("persisted triggered_rule = %v, want amount_threshold", gotTriggered)
	}
}

func TestCheckTransfer_VelocityCount_Reject(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	accountID := randomUUID(t)
	cleanupFraudChecks(t, pool, accountID)

	const smallAmount = 1000 // well under amount_threshold and velocity_sum
	var lastTransferID, lastDecision, lastTriggered string
	for i := 1; i <= 6; i++ { // velocity_count threshold is 5: the 6th transfer must reject
		transferID := randomUUID(t)
		decision, triggeredRule, _, err := checkTransfer(ctx, pool, transferID, accountID, smallAmount)
		if err != nil {
			t.Fatalf("checkTransfer call %d: %v", i, err)
		}
		lastTransferID, lastDecision, lastTriggered = transferID, decision, triggeredRule
	}

	if lastDecision != "reject" {
		t.Errorf("6th call decision = %q, want reject", lastDecision)
	}
	if lastTriggered != "velocity_count" {
		t.Errorf("6th call triggeredRule = %q, want velocity_count", lastTriggered)
	}

	gotDecision, gotTriggered, _ := fetchFraudCheck(t, ctx, pool, lastTransferID)
	if gotDecision != "reject" || gotTriggered == nil || *gotTriggered != "velocity_count" {
		t.Errorf("persisted 6th row = (%q, %v), want (reject, velocity_count)", gotDecision, gotTriggered)
	}
}

func TestCheckTransfer_VelocitySum_Reject(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	accountID := randomUUID(t)
	cleanupFraudChecks(t, pool, accountID)

	// Each transfer stays under amount_threshold (500000) individually, but
	// three of them push the cumulative sum over velocity_sum (1000000)
	// while staying well under velocity_count's 5-transfer cap, so
	// velocity_sum is the one that trips.
	const amount = 400000
	var lastTransferID, lastDecision, lastTriggered string
	for i := 1; i <= 3; i++ {
		transferID := randomUUID(t)
		decision, triggeredRule, _, err := checkTransfer(ctx, pool, transferID, accountID, amount)
		if err != nil {
			t.Fatalf("checkTransfer call %d: %v", i, err)
		}
		lastTransferID, lastDecision, lastTriggered = transferID, decision, triggeredRule
	}

	if lastDecision != "reject" {
		t.Errorf("3rd call decision = %q, want reject", lastDecision)
	}
	if lastTriggered != "velocity_sum" {
		t.Errorf("3rd call triggeredRule = %q, want velocity_sum", lastTriggered)
	}

	gotDecision, gotTriggered, _ := fetchFraudCheck(t, ctx, pool, lastTransferID)
	if gotDecision != "reject" || gotTriggered == nil || *gotTriggered != "velocity_sum" {
		t.Errorf("persisted 3rd row = (%q, %v), want (reject, velocity_sum)", gotDecision, gotTriggered)
	}
}

func TestCheckTransfer_Approve(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	accountID := randomUUID(t)
	transferID := randomUUID(t)
	cleanupFraudChecks(t, pool, accountID)

	decision, triggeredRule, reason, err := checkTransfer(ctx, pool, transferID, accountID, 1000)
	if err != nil {
		t.Fatalf("checkTransfer: %v", err)
	}
	if decision != "approve" {
		t.Errorf("decision = %q, want approve", decision)
	}
	if triggeredRule != "" {
		t.Errorf("triggeredRule = %q, want empty", triggeredRule)
	}
	if reason == "" {
		t.Error("reason is empty, want an explanation even on approve")
	}

	gotDecision, gotTriggered, gotDetails := fetchFraudCheck(t, ctx, pool, transferID)
	if gotDecision != "approve" {
		t.Errorf("persisted decision = %q, want approve", gotDecision)
	}
	if gotTriggered != nil {
		t.Errorf("persisted triggered_rule = %v, want NULL", gotTriggered)
	}

	var details map[string]any
	if err := json.Unmarshal(gotDetails, &details); err != nil {
		t.Fatalf("details is not valid JSON: %v", err)
	}
	if len(details) == 0 {
		t.Error("details is empty, want observed values per evaluated rule")
	}
}

func TestCheckTransfer_DisabledRuleSkipped(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	accountID := randomUUID(t)
	transferID := randomUUID(t)
	cleanupFraudChecks(t, pool, accountID)

	if _, err := pool.Exec(ctx, "UPDATE fraud_rules SET enabled = false WHERE rule_type = 'amount_threshold'"); err != nil {
		t.Fatalf("disable amount_threshold rule: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "UPDATE fraud_rules SET enabled = true WHERE rule_type = 'amount_threshold'"); err != nil {
			t.Logf("cleanup: re-enable amount_threshold rule: %v", err)
		}
	})

	// Would reject under amount_threshold (500000) if the rule were enabled.
	decision, triggeredRule, _, err := checkTransfer(ctx, pool, transferID, accountID, 600000)
	if err != nil {
		t.Fatalf("checkTransfer: %v", err)
	}
	if decision != "approve" {
		t.Errorf("decision = %q, want approve (rule disabled)", decision)
	}
	if triggeredRule != "" {
		t.Errorf("triggeredRule = %q, want empty", triggeredRule)
	}
}

func TestCheckTransfer_FailsClosedOnError(t *testing.T) {
	pool := newTestPool(t)
	accountID := randomUUID(t)
	transferID := randomUUID(t)
	cleanupFraudChecks(t, pool, accountID)

	// An already-canceled context makes every query fail immediately,
	// simulating "fraud-svc can't compute an answer" without needing to
	// actually take Postgres down.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, _, err := checkTransfer(ctx, pool, transferID, accountID, 1000)
	if err == nil {
		t.Fatal("checkTransfer with a canceled context returned no error (fail-open), want an error")
	}

	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM fraud_checks WHERE transfer_id = $1", transferID,
	).Scan(&count); err != nil {
		t.Fatalf("count fraud_checks rows: %v", err)
	}
	if count != 0 {
		t.Errorf("fraud_checks rows for a failed check = %d, want 0 (no partial write on error)", count)
	}
}
