package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// outboxRow is a minimal projection of one outbox row, for tests asserting
// what markTransferCompleted/Failed/Rejected (or their *IfPending
// counterparts) actually wrote alongside a transfers status UPDATE.
type outboxRow struct {
	EventID      string
	EventType    string
	PartitionKey string
	Payload      []byte
	PublishedAt  *time.Time
}

// getOutboxRow fetches the one outbox row expected for partitionKey
// (sender_account_id) — every test in this package uses a fresh random
// sender id, so at most one row should ever match.
func getOutboxRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, partitionKey string) outboxRow {
	t.Helper()
	var row outboxRow
	err := pool.QueryRow(ctx,
		"SELECT event_id, event_type, partition_key, payload, published_at FROM outbox WHERE partition_key = $1",
		partitionKey,
	).Scan(&row.EventID, &row.EventType, &row.PartitionKey, &row.Payload, &row.PublishedAt)
	if err != nil {
		t.Fatalf("get outbox row partition_key=%s: %v", partitionKey, err)
	}
	return row
}

// outboxRowCount returns how many outbox rows exist for partitionKey, used
// to assert that a non-definite outcome (uncertain settlement, approved or
// uncertain fraud check) wrote nothing to outbox at all.
func outboxRowCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, partitionKey string) int {
	t.Helper()
	var count int
	err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM outbox WHERE partition_key = $1", partitionKey).Scan(&count)
	if err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	return count
}

// deleteOutboxRows cleans up outbox rows written for partitionKey by a
// test. outbox rows aren't linked to a transfer's idempotency_key, so they
// need their own cleanup alongside deleteTransfer — see
// insertPendingTransfer/insertPendingTransferWithKey.
func deleteOutboxRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, partitionKey string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "DELETE FROM outbox WHERE partition_key = $1", partitionKey); err != nil {
		t.Logf("cleanup: delete outbox rows partition_key=%s: %v", partitionKey, err)
	}
}

// forceOutboxInsertsToFail makes any INSERT INTO outbox fail for the rest
// of the test, to prove markTransferCompleted/Failed/Rejected (and their
// *IfPending counterparts) roll back their status UPDATE when the outbox
// write in the same transaction can't be committed — the entire reason
// both writes happen in one transaction. NOT VALID skips validating rows
// already in the table (so this succeeds no matter what other tests have
// left behind) but still enforces the constraint against every new row,
// so every subsequent INSERT fails immediately. Tests in this package never
// run in parallel (no t.Parallel() calls), so a table-wide constraint like
// this can't bleed into an unrelated concurrently-running test.
func forceOutboxInsertsToFail(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	const constraintName = "outbox_test_force_fail"
	if _, err := pool.Exec(ctx, "ALTER TABLE outbox ADD CONSTRAINT "+constraintName+" CHECK (false) NOT VALID"); err != nil {
		t.Fatalf("add force-fail constraint: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, "ALTER TABLE outbox DROP CONSTRAINT "+constraintName); err != nil {
			t.Logf("cleanup: drop force-fail constraint: %v", err)
		}
	})
}

func TestMarkTransferCompleted_OutboxInsertFailureRollsBackStatusUpdate(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	pending := insertPendingTransfer(t, ctx, pool, randomUUIDForTest(t), randomUUIDForTest(t), 1000)
	forceOutboxInsertsToFail(t, ctx, pool)

	if _, err := markTransferCompleted(ctx, pool, pending.ID, randomUUIDForTest(t)); err == nil {
		t.Fatal("markTransferCompleted: want error when the outbox insert is forced to fail, got nil")
	}

	row := getTransferByID(t, ctx, pool, pending.ID)
	if row.Status != "pending" {
		t.Errorf("row.Status = %q, want \"pending\" (the status UPDATE must roll back when the outbox insert in the same transaction fails)", row.Status)
	}
	if got := outboxRowCount(t, ctx, pool, pending.SenderAccountID); got != 0 {
		t.Errorf("outbox rows for sender=%s = %d, want 0 (a failed insert must not leave a partial row)", pending.SenderAccountID, got)
	}
}

func TestMarkTransferRejected_OutboxInsertFailureRollsBackStatusUpdate(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	pending := insertPendingTransfer(t, ctx, pool, randomUUIDForTest(t), randomUUIDForTest(t), 1000)
	forceOutboxInsertsToFail(t, ctx, pool)

	if _, err := markTransferRejected(ctx, pool, pending.ID, "amount_threshold"); err == nil {
		t.Fatal("markTransferRejected: want error when the outbox insert is forced to fail, got nil")
	}

	row := getTransferByID(t, ctx, pool, pending.ID)
	if row.Status != "pending" {
		t.Errorf("row.Status = %q, want \"pending\" (rollback)", row.Status)
	}
	if got := outboxRowCount(t, ctx, pool, pending.SenderAccountID); got != 0 {
		t.Errorf("outbox rows = %d, want 0", got)
	}
}

func TestMarkTransferCompletedIfPending_OutboxInsertFailureRollsBackStatusUpdate(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	pending := insertPendingTransfer(t, ctx, pool, randomUUIDForTest(t), randomUUIDForTest(t), 1000)
	forceOutboxInsertsToFail(t, ctx, pool)

	if _, err := markTransferCompletedIfPending(ctx, pool, pending, randomUUIDForTest(t)); err == nil {
		t.Fatal("markTransferCompletedIfPending: want error when the outbox insert is forced to fail, got nil")
	}

	row := getTransferByID(t, ctx, pool, pending.ID)
	if row.Status != "pending" {
		t.Errorf("row.Status = %q, want \"pending\" (rollback — the reconciliation writer must be just as atomic as the request-path ones)", row.Status)
	}
	if got := outboxRowCount(t, ctx, pool, pending.SenderAccountID); got != 0 {
		t.Errorf("outbox rows = %d, want 0", got)
	}
}
