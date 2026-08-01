package main

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

// reconcileInterval is how often the worker looks for stale pending
// transfers. Fixed rather than configurable (unlike staleAfter, below) —
// there's no operational reason to tune how often a cheap poll runs, only
// how old a transfer must be before it's worth polling for.
const reconcileInterval = 30 * time.Second

// runReconciliationWorker is the saga-compensation step this service was
// missing: settleTransfer's own doc comment already explains why a
// transport failure after ledger-svc's ExecuteTransfer call leaves a
// transfer stuck in 'pending' forever, unresolved by any request path —
// ledger-svc's ExecuteTransfer commits its own transaction (or doesn't)
// before any gRPC response is sent, so if the response never arrives,
// nobody knows which happened.
//
// There is no rollback here, because there is nothing to roll back:
// entries are append-only, and if the transfer really did execute,
// undoing it would require a genuine reversing entry — a real compensating
// transaction, not implemented here because it isn't needed yet (see
// README). The two facts this worker can actually establish, by asking
// ledger-svc's own data instead of guessing, are exhaustive for a
// stuck-pending row:
//   - ledger-svc has an entry tagged with this transfer's id: it executed.
//     Nothing to compensate — just record what already happened
//     (completed).
//   - ledger-svc has no such entry: it never executed. No money moved, so
//     there is nothing to compensate either (failed).
//
// Like every background loop in this repo (accounts-svc's Kafka consumer
// being the other example), this runs for the lifetime of the process with
// no graceful shutdown — ctx is context.Background() from main(), and the
// loop just stops when the process does.
func runReconciliationWorker(ctx context.Context, pool *pgxpool.Pool, ledgerClient ledgerv1.LedgerServiceClient, staleAfter time.Duration) {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcileOnce(ctx, pool, ledgerClient, staleAfter)
		}
	}
}

func reconcileOnce(ctx context.Context, pool *pgxpool.Pool, ledgerClient ledgerv1.LedgerServiceClient, staleAfter time.Duration) {
	stale, err := getStalePendingTransfers(ctx, pool, staleAfter)
	if err != nil {
		log.Printf("transfers-svc: reconcile: list stale pending transfers: %v", err)
		return
	}
	for _, transfer := range stale {
		reconcileTransfer(ctx, pool, ledgerClient, transfer)
	}
}

// reconcileTransfer resolves a single stale-pending transfer. It always
// writes via the *IfPending helpers (transfer.go) — a concurrent ordinary
// request could have resolved this same transfer between
// getStalePendingTransfers reading it and this call writing to it (e.g. a
// client retry that this time reached settleTransfer for real), and that
// result must win, not be silently overwritten by this worker's slightly
// stale view.
func reconcileTransfer(ctx context.Context, pool *pgxpool.Pool, ledgerClient ledgerv1.LedgerServiceClient, transfer Transfer) {
	resp, err := ledgerClient.GetTransactionByReference(ctx, &ledgerv1.GetTransactionByReferenceRequest{Reference: transfer.ID})
	if err != nil {
		log.Printf("transfers-svc: reconcile: transfer %s: GetTransactionByReference: %v (will retry next tick)", transfer.ID, err)
		return
	}

	if resp.GetFound() {
		resolved, err := markTransferCompletedIfPending(ctx, pool, transfer, resp.GetTransactionId())
		if err != nil {
			log.Printf("transfers-svc: reconcile: transfer %s: mark completed: %v", transfer.ID, err)
			return
		}
		if resolved {
			log.Printf("transfers-svc: reconcile: transfer %s resolved to completed (ledger_transaction_id=%s) — ledger-svc had already executed it, the original response was never received", transfer.ID, resp.GetTransactionId())
		}
		return
	}

	resolved, err := markTransferFailedIfPending(ctx, pool, transfer, failureReasonTimeoutUnresolved)
	if err != nil {
		log.Printf("transfers-svc: reconcile: transfer %s: mark failed: %v", transfer.ID, err)
		return
	}
	if resolved {
		log.Printf("transfers-svc: reconcile: transfer %s resolved to failed (reason=%s) — ledger-svc never executed it, no money moved", transfer.ID, failureReasonTimeoutUnresolved)
	}
}
