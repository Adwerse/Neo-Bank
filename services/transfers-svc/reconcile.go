package main

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"

	"neobank/pkg/tracing"
	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

// tracerScope names this service's instrumentation scope for the spans
// it creates by hand.
const tracerScope = "neobank/services/transfers-svc"

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
// This same worker also drives deposit reconciliation (reconcileDepositsOnce,
// deposit_reconcile.go) on every tick — including, notably, the everyday
// crediting of Stripe-confirmed deposits, not just resolving stuck ones.
// See README, "Депозиты: почему фоновый воркер" for why that lives here
// rather than in the webhook handler or a separate task queue this repo
// doesn't otherwise have.
//
// Like every background loop in this repo (accounts-svc's Kafka consumer
// being the other example), this runs for the lifetime of the process with
// no graceful shutdown — ctx is context.Background() from main(), and the
// loop just stops when the process does.
func runReconciliationWorker(ctx context.Context, pool *pgxpool.Pool, ledgerClient ledgerv1.LedgerServiceClient, staleAfter time.Duration, paymentIntents stripePaymentIntentRetriever, depositStaleAfter time.Duration) {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcileOnce(ctx, pool, ledgerClient, staleAfter)
			reconcileDepositsOnce(ctx, pool, ledgerClient, paymentIntents, depositStaleAfter)
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
	// The demonstration this sprint is built around. This worker woke on a
	// timer; nothing called it, so its span has no parent by construction.
	// Linking it to the trace stored when the transfer row was created
	// makes the whole story of a stuck transfer navigable in Jaeger: the
	// original request, the point where it stopped short of a terminal
	// state, and — minutes later, in a separate trace — the worker that
	// finished it.
	//
	// A link, not a parent, for the reason spelled out in
	// tracing.StartLinkedRoot: the request span ended minutes ago, and
	// nesting this inside it would claim a 50ms operation contains a child
	// that started long after it returned.
	ctx, span := tracing.StartLinkedRoot(ctx, tracerScope, "reconcile transfer",
		originTraceContext(ctx, pool, "transfers", transfer.ID),
		tracing.TransferID(transfer.ID),
		tracing.AmountMinor(transfer.Amount),
		attribute.String("neobank.reconcile.trigger", "stale_pending"),
		attribute.Float64("neobank.reconcile.stale_for_seconds", time.Since(transfer.UpdatedAt).Seconds()),
	)
	defer span.End()

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
			tracing.SetAttributes(ctx,
				tracing.TransferStatus("completed"),
				tracing.LedgerTransactionID(resp.GetTransactionId()),
				attribute.String("neobank.reconcile.resolution", "completed"),
			)
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
		tracing.SetAttributes(ctx,
			tracing.TransferStatus("failed"),
			attribute.String("neobank.reconcile.resolution", "failed"),
		)
	}
}
