package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

// backdateTransferUpdatedAt directly rewrites updated_at, simulating a
// transfer that has sat pending for age — insertPendingTransfer always
// leaves updated_at at "now," so tests need this to exercise
// getStalePendingTransfers' age threshold without actually waiting.
func backdateTransferUpdatedAt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string, age time.Duration) {
	t.Helper()
	_, err := pool.Exec(ctx, "UPDATE transfers SET updated_at = $1 WHERE id = $2", time.Now().Add(-age), id)
	if err != nil {
		t.Fatalf("backdate transfer updated_at: %v", err)
	}
}

func TestGetStalePendingTransfers(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	stale := insertPendingTransfer(t, ctx, pool, randomUUIDForTest(t), randomUUIDForTest(t), 1000)
	backdateTransferUpdatedAt(t, ctx, pool, stale.ID, 5*time.Minute)

	fresh := insertPendingTransfer(t, ctx, pool, randomUUIDForTest(t), randomUUIDForTest(t), 1000)

	results, err := getStalePendingTransfers(ctx, pool, 2*time.Minute)
	if err != nil {
		t.Fatalf("getStalePendingTransfers: unexpected error: %v", err)
	}

	var foundStale, foundFresh bool
	for _, r := range results {
		if r.ID == stale.ID {
			foundStale = true
		}
		if r.ID == fresh.ID {
			foundFresh = true
		}
	}
	if !foundStale {
		t.Error("getStalePendingTransfers did not return the stale (5m old) transfer")
	}
	if foundFresh {
		t.Error("getStalePendingTransfers returned the fresh (0s old) transfer, want excluded")
	}
}

func TestMarkTransferCompletedIfPending_SkipsAlreadyResolved(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	pending := insertPendingTransfer(t, ctx, pool, randomUUIDForTest(t), randomUUIDForTest(t), 1000)
	// Simulate a concurrent ordinary request having already resolved this
	// transfer between getStalePendingTransfers reading it and the worker
	// writing to it.
	if _, err := markTransferFailed(ctx, pool, pending.ID, failureReasonInsufficientFunds); err != nil {
		t.Fatalf("markTransferFailed (setup): %v", err)
	}

	resolved, err := markTransferCompletedIfPending(ctx, pool, pending, randomUUIDForTest(t))
	if err != nil {
		t.Fatalf("markTransferCompletedIfPending: unexpected error: %v", err)
	}
	if resolved {
		t.Error("markTransferCompletedIfPending reported resolved=true for an already-failed transfer, want false (must not overwrite)")
	}

	row := getTransferByID(t, ctx, pool, pending.ID)
	if row.Status != "failed" {
		t.Errorf("row.Status = %q, want \"failed\" (unchanged — the race guard must not overwrite an already-resolved transfer)", row.Status)
	}
	if row.FailureReason == nil || *row.FailureReason != failureReasonInsufficientFunds {
		t.Errorf("row.FailureReason = %v, want %q (unchanged)", row.FailureReason, failureReasonInsufficientFunds)
	}
	// Exactly 1: the setup markTransferFailed call's own event — the skipped
	// markTransferCompletedIfPending call above must not have added another.
	if got := outboxRowCount(t, ctx, pool, pending.SenderAccountID); got != 1 {
		t.Errorf("outbox rows = %d, want 1 (the skipped call must not write its own event)", got)
	}
}

func TestMarkTransferFailedIfPending_SkipsAlreadyResolved(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	pending := insertPendingTransfer(t, ctx, pool, randomUUIDForTest(t), randomUUIDForTest(t), 1000)
	wantTransactionID := randomUUIDForTest(t)
	if _, err := markTransferCompleted(ctx, pool, pending.ID, wantTransactionID); err != nil {
		t.Fatalf("markTransferCompleted (setup): %v", err)
	}

	resolved, err := markTransferFailedIfPending(ctx, pool, pending, failureReasonTimeoutUnresolved)
	if err != nil {
		t.Fatalf("markTransferFailedIfPending: unexpected error: %v", err)
	}
	if resolved {
		t.Error("markTransferFailedIfPending reported resolved=true for an already-completed transfer, want false (must not overwrite)")
	}

	row := getTransferByID(t, ctx, pool, pending.ID)
	if row.Status != "completed" {
		t.Errorf("row.Status = %q, want \"completed\" (unchanged)", row.Status)
	}
	if row.LedgerTransactionID == nil || *row.LedgerTransactionID != wantTransactionID {
		t.Errorf("row.LedgerTransactionID = %v, want %q (unchanged)", row.LedgerTransactionID, wantTransactionID)
	}
	// Exactly 1: the setup markTransferCompleted call's own event — the
	// skipped markTransferFailedIfPending call above must not add another.
	if got := outboxRowCount(t, ctx, pool, pending.SenderAccountID); got != 1 {
		t.Errorf("outbox rows = %d, want 1 (the skipped call must not write its own event)", got)
	}
}

func TestReconcileTransfer_LedgerExecutedIt(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	pending := insertPendingTransfer(t, ctx, pool, randomUUIDForTest(t), randomUUIDForTest(t), 1000)
	wantTransactionID := randomUUIDForTest(t)

	ledgerClient := &fakeLedgerClient{
		getTransactionByReferenceFunc: func(ctx context.Context, req *ledgerv1.GetTransactionByReferenceRequest) (*ledgerv1.GetTransactionByReferenceResponse, error) {
			if req.GetReference() != pending.ID {
				t.Errorf("GetTransactionByReference reference = %q, want %q", req.GetReference(), pending.ID)
			}
			return &ledgerv1.GetTransactionByReferenceResponse{Found: true, TransactionId: wantTransactionID}, nil
		},
	}

	reconcileTransfer(ctx, pool, ledgerClient, pending)

	row := getTransferByID(t, ctx, pool, pending.ID)
	if row.Status != "completed" {
		t.Errorf("row.Status = %q, want \"completed\"", row.Status)
	}
	if row.LedgerTransactionID == nil || *row.LedgerTransactionID != wantTransactionID {
		t.Errorf("row.LedgerTransactionID = %v, want %q", row.LedgerTransactionID, wantTransactionID)
	}
	if got := outboxRowCount(t, ctx, pool, pending.SenderAccountID); got != 1 {
		t.Errorf("outbox rows = %d, want 1 (reconciliation resolving a transfer must publish an event too)", got)
	}
}

func TestReconcileTransfer_LedgerNeverExecutedIt(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	pending := insertPendingTransfer(t, ctx, pool, randomUUIDForTest(t), randomUUIDForTest(t), 1000)

	ledgerClient := &fakeLedgerClient{
		getTransactionByReferenceFunc: func(ctx context.Context, req *ledgerv1.GetTransactionByReferenceRequest) (*ledgerv1.GetTransactionByReferenceResponse, error) {
			return &ledgerv1.GetTransactionByReferenceResponse{Found: false}, nil
		},
	}

	reconcileTransfer(ctx, pool, ledgerClient, pending)

	row := getTransferByID(t, ctx, pool, pending.ID)
	if row.Status != "failed" {
		t.Errorf("row.Status = %q, want \"failed\"", row.Status)
	}
	if row.FailureReason == nil || *row.FailureReason != failureReasonTimeoutUnresolved {
		t.Errorf("row.FailureReason = %v, want %q", row.FailureReason, failureReasonTimeoutUnresolved)
	}
	if row.LedgerTransactionID != nil {
		t.Errorf("row.LedgerTransactionID = %v, want nil", row.LedgerTransactionID)
	}
	if got := outboxRowCount(t, ctx, pool, pending.SenderAccountID); got != 1 {
		t.Errorf("outbox rows = %d, want 1", got)
	}
}

func TestReconcileTransfer_TransportErrorLeavesRowUntouched(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	pending := insertPendingTransfer(t, ctx, pool, randomUUIDForTest(t), randomUUIDForTest(t), 1000)

	ledgerClient := &fakeLedgerClient{
		getTransactionByReferenceFunc: func(ctx context.Context, req *ledgerv1.GetTransactionByReferenceRequest) (*ledgerv1.GetTransactionByReferenceResponse, error) {
			return nil, context.DeadlineExceeded
		},
	}

	// reconcileTransfer logs and returns on a transport error — it must not
	// guess at a terminal state; the next tick will try again.
	reconcileTransfer(ctx, pool, ledgerClient, pending)

	row := getTransferByID(t, ctx, pool, pending.ID)
	if row.Status != "pending" {
		t.Errorf("row.Status = %q, want \"pending\" (unchanged — a transport error must not resolve anything)", row.Status)
	}
	if got := outboxRowCount(t, ctx, pool, pending.SenderAccountID); got != 0 {
		t.Errorf("outbox rows = %d, want 0", got)
	}
}
