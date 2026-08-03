package main

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v86"

	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

// depositDivergenceLogPrefix marks the one log line in this file meant to
// be alertable — everything else here is routine reconciliation chatter.
// A "succeeded but not credited" deposit older than staleAfter is exactly
// the invariant this sprint centers on: Stripe took the money and the
// ledger hasn't reflected it, and that must never go unnoticed. In a real
// bank this would page someone; here, a distinctly greppable log line is
// the deliverable — see README for how an operator would act on it.
const depositDivergenceLogPrefix = "transfers-svc: DIVERGENCE ALERT"

// stripePaymentIntentRetriever is the one Stripe SDK method
// reconcilePendingDepositsWithIntent needs, narrowed to an interface for
// the same reason as stripePaymentIntentCreator (deposit.go) — tests
// substitute a fake instead of a live Stripe account.
// stripeClient.V1PaymentIntents satisfies this too, alongside
// stripePaymentIntentCreator.
type stripePaymentIntentRetriever interface {
	Retrieve(ctx context.Context, id string, params *stripe.PaymentIntentRetrieveParams) (*stripe.PaymentIntent, error)
}

// reconcileDepositsOnce runs all three deposit reconciliation categories
// described in README, once per tick of runReconciliationWorker (this
// file extends that existing worker rather than running its own — see
// the worker-choice writeup in README for why). Called after reconcileOnce
// (transfers) on the same tick; the two are independent concerns sharing
// one ticker purely to avoid a second goroutine and timer for what is,
// at this scale, a cheap poll either way.
//
//  1. creditSucceededDeposits: the PRIMARY path that turns 'succeeded'
//     into 'credited' — not just a fallback for stuck ones. Deliberately
//     unconditional (no staleness gate): ledger.Deposit is idempotent, so
//     retrying it every tick for every not-yet-credited deposit is safe,
//     and this is where the money actually moves. See README for why
//     this lives in a poller rather than the webhook handler or a queued
//     async task.
//  2. reconcilePendingDepositsWithIntent: a 'pending' deposit that HAS a
//     stripe_payment_intent_id but hasn't heard from a webhook in
//     staleAfter is resolved by asking Stripe directly (PaymentIntent
//     Retrieve) — the standard "webhook is the fast path, polling is the
//     reliable fallback" pattern.
//  3. reconcileOrphanedPendingDeposits: a 'pending' deposit with NO
//     stripe_payment_intent_id after staleAfter is the crash-between-
//     insert-and-intent case createDeposit's own doc comment already
//     calls out as safe-but-abandoned; this is what actually cleans it
//     up, by marking it failed.
func reconcileDepositsOnce(ctx context.Context, pool *pgxpool.Pool, ledgerClient ledgerv1.LedgerServiceClient, paymentIntents stripePaymentIntentRetriever, staleAfter time.Duration) {
	creditSucceededDeposits(ctx, pool, ledgerClient, staleAfter)
	reconcilePendingDepositsWithIntent(ctx, pool, paymentIntents, staleAfter)
	reconcileOrphanedPendingDeposits(ctx, pool, staleAfter)
}

func creditSucceededDeposits(ctx context.Context, pool *pgxpool.Pool, ledgerClient ledgerv1.LedgerServiceClient, divergenceAfter time.Duration) {
	deposits, err := getSucceededDeposits(ctx, pool)
	if err != nil {
		log.Printf("transfers-svc: reconcile deposits: list succeeded deposits: %v", err)
		return
	}
	for _, d := range deposits {
		creditDeposit(ctx, pool, ledgerClient, d, divergenceAfter)
	}
}

// creditDeposit attempts to move one 'succeeded' deposit to 'credited'.
// The divergence check runs unconditionally, before the attempt: even a
// credit that succeeds on this very tick should still have raised the
// alert if it had already been stuck past divergenceAfter — the alert is
// about how long the invariant was violated, not about whether this
// particular attempt happened to fix it.
func creditDeposit(ctx context.Context, pool *pgxpool.Pool, ledgerClient ledgerv1.LedgerServiceClient, d Deposit, divergenceAfter time.Duration) {
	if time.Since(d.UpdatedAt) > divergenceAfter {
		log.Printf("%s: deposit %s has been 'succeeded' (Stripe confirmed the charge) for over %s without becoming 'credited' — money was taken but not yet reflected in the user's balance; check ledger-svc connectivity", depositDivergenceLogPrefix, d.ID, divergenceAfter)
	}

	// reference = d.ID: idempotent on ledger-svc's side (see
	// postUncheckedTransfer in ledger-svc/ledger.go), so calling this
	// again for a deposit already credited by a previous tick (or a
	// concurrent replica) safely returns the existing transaction rather
	// than posting a second one.
	resp, err := ledgerClient.Deposit(ctx, &ledgerv1.DepositRequest{AccountId: d.AccountID, Amount: d.Amount, Reference: d.ID})
	if err != nil {
		log.Printf("transfers-svc: reconcile deposits: deposit %s: ledger.Deposit: %v (will retry next tick)", d.ID, err)
		return
	}

	credited, err := markDepositCreditedIfSucceeded(ctx, pool, d.ID, resp.GetTransactionId())
	if err != nil {
		log.Printf("transfers-svc: reconcile deposits: deposit %s: mark credited: %v", d.ID, err)
		return
	}
	if credited {
		log.Printf("transfers-svc: reconcile deposits: deposit %s credited (ledger_transaction_id=%s)", d.ID, resp.GetTransactionId())
	}
	// !credited means a concurrent caller (another replica's tick) already
	// credited this deposit between getSucceededDeposits reading it and
	// this write — not an error, nothing left to do.
}

func reconcilePendingDepositsWithIntent(ctx context.Context, pool *pgxpool.Pool, paymentIntents stripePaymentIntentRetriever, staleAfter time.Duration) {
	deposits, err := getPendingDepositsWithIntentBefore(ctx, pool, time.Now().Add(-staleAfter))
	if err != nil {
		log.Printf("transfers-svc: reconcile deposits: list pending deposits with intent: %v", err)
		return
	}
	for _, d := range deposits {
		reconcilePendingDepositWithIntent(ctx, pool, paymentIntents, d)
	}
}

// reconcilePendingDepositWithIntent asks Stripe directly for a deposit's
// PaymentIntent status — the fallback for a webhook that likely never
// arrived. Only two of Stripe's PaymentIntent statuses resolve anything
// here (succeeded, canceled); every other status (requires_payment_method,
// requires_action, processing, etc.) means the payment is still
// genuinely in progress, so this deposit is simply left pending and
// checked again next tick.
func reconcilePendingDepositWithIntent(ctx context.Context, pool *pgxpool.Pool, paymentIntents stripePaymentIntentRetriever, d Deposit) {
	if d.StripePaymentIntentID == nil {
		return // getPendingDepositsWithIntentBefore already filters this; defensive only.
	}

	intent, err := paymentIntents.Retrieve(ctx, *d.StripePaymentIntentID, nil)
	if err != nil {
		log.Printf("transfers-svc: reconcile deposits: deposit %s: stripe retrieve %s: %v (will retry next tick)", d.ID, *d.StripePaymentIntentID, err)
		return
	}

	switch intent.Status {
	case stripe.PaymentIntentStatusSucceeded:
		marked, err := markDepositSucceededIfPending(ctx, pool, *d.StripePaymentIntentID)
		if err != nil {
			log.Printf("transfers-svc: reconcile deposits: deposit %s: mark succeeded: %v", d.ID, err)
			return
		}
		if marked {
			log.Printf("transfers-svc: reconcile deposits: deposit %s resolved to succeeded via Stripe polling — the webhook for this payment was likely lost", d.ID)
		}
	case stripe.PaymentIntentStatusCanceled:
		marked, err := markDepositFailedIfPending(ctx, pool, d.ID, "canceled")
		if err != nil {
			log.Printf("transfers-svc: reconcile deposits: deposit %s: mark failed: %v", d.ID, err)
			return
		}
		if marked {
			log.Printf("transfers-svc: reconcile deposits: deposit %s resolved to failed (canceled) via Stripe polling", d.ID)
		}
	default:
		// Still in progress — nothing to do yet.
	}
}

func reconcileOrphanedPendingDeposits(ctx context.Context, pool *pgxpool.Pool, staleAfter time.Duration) {
	deposits, err := getOrphanedPendingDeposits(ctx, pool, time.Now().Add(-staleAfter))
	if err != nil {
		log.Printf("transfers-svc: reconcile deposits: list orphaned pending deposits: %v", err)
		return
	}
	for _, d := range deposits {
		marked, err := markDepositFailedIfOrphaned(ctx, pool, d.ID, "abandoned_before_payment_intent")
		if err != nil {
			log.Printf("transfers-svc: reconcile deposits: deposit %s: mark failed: %v", d.ID, err)
			continue
		}
		if marked {
			log.Printf("transfers-svc: reconcile deposits: deposit %s resolved to failed — abandoned before a Stripe PaymentIntent was ever created, no money moved", d.ID)
		}
	}
}
