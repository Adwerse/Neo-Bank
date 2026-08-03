package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v86"

	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

// fakePaymentIntentRetriever implements stripePaymentIntentRetriever
// without a live Stripe account — same fake-by-function-field shape as
// fakePaymentIntentCreator (webhook_test.go).
type fakePaymentIntentRetriever struct {
	retrieveFunc func(ctx context.Context, id string, params *stripe.PaymentIntentRetrieveParams) (*stripe.PaymentIntent, error)
}

func (f *fakePaymentIntentRetriever) Retrieve(ctx context.Context, id string, params *stripe.PaymentIntentRetrieveParams) (*stripe.PaymentIntent, error) {
	return f.retrieveFunc(ctx, id, params)
}

// insertDepositForReconcile inserts a deposits row directly with an
// explicit status and stripe_payment_intent_id (nil for none), bypassing
// createDeposit/Stripe entirely — deposit_reconcile.go's functions only
// care about a row's current state, not how it got there. Registers
// cleanup.
func insertDepositForReconcile(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID string, amount int64, status string, stripePaymentIntentID *string) string {
	t.Helper()
	var depositID string
	err := pool.QueryRow(ctx,
		"INSERT INTO deposits (account_id, amount, status, stripe_payment_intent_id) VALUES ($1, $2, $3, $4) RETURNING id",
		accountID, amount, status, stripePaymentIntentID,
	).Scan(&depositID)
	if err != nil {
		t.Fatalf("insert deposit (status=%s): %v", status, err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM deposits WHERE id = $1", depositID); err != nil {
			t.Logf("cleanup: delete deposit id=%s: %v", depositID, err)
		}
	})
	return depositID
}

// ageDeposit backdates a deposit's updated_at so staleness-gated
// reconciliation queries (getPendingDepositsWithIntentBefore,
// getOrphanedPendingDeposits) pick it up without a real wait.
func ageDeposit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, depositID string, age time.Duration) {
	t.Helper()
	if _, err := pool.Exec(ctx, "UPDATE deposits SET updated_at = $1 WHERE id = $2", time.Now().Add(-age), depositID); err != nil {
		t.Fatalf("backdate deposit %s: %v", depositID, err)
	}
}

func TestCreditSucceededDeposits_Success(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUIDForTest(t)
	intentID := "pi_" + randomUUIDForTest(t)
	depositID := insertDepositForReconcile(t, ctx, pool, accountID, 5000, "succeeded", &intentID)
	// markDepositCreditedIfSucceeded writes an outbox row keyed by
	// accountID (the partition_key) alongside the deposits UPDATE — it
	// isn't linked to depositID, so it needs its own cleanup, same as
	// every other outbox-writing test in this package (see
	// deleteOutboxRows's doc comment in outbox_test.go).
	t.Cleanup(func() { deleteOutboxRows(t, context.Background(), pool, accountID) })

	wantTxnID := randomUUIDForTest(t)
	ledgerClient := &fakeLedgerClient{
		depositFunc: func(ctx context.Context, req *ledgerv1.DepositRequest) (*ledgerv1.DepositResponse, error) {
			if req.GetAccountId() != accountID || req.GetAmount() != 5000 || req.GetReference() != depositID {
				t.Errorf("Deposit request = %+v, want account=%s amount=5000 reference=%s", req, accountID, depositID)
			}
			return &ledgerv1.DepositResponse{TransactionId: wantTxnID}, nil
		},
	}

	creditSucceededDeposits(ctx, pool, ledgerClient, time.Hour)

	var status string
	var ledgerTxnID *string
	if err := pool.QueryRow(ctx, "SELECT status, ledger_transaction_id FROM deposits WHERE id = $1", depositID).Scan(&status, &ledgerTxnID); err != nil {
		t.Fatalf("query deposit: %v", err)
	}
	if status != "credited" {
		t.Errorf("status = %q, want %q", status, "credited")
	}
	if ledgerTxnID == nil || *ledgerTxnID != wantTxnID {
		t.Errorf("ledger_transaction_id = %v, want %s", ledgerTxnID, wantTxnID)
	}

	var outboxCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM outbox WHERE event_type = 'DepositCredited' AND partition_key = $1", accountID).Scan(&outboxCount); err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	if outboxCount != 1 {
		t.Errorf("DepositCredited outbox rows for account %s = %d, want 1", accountID, outboxCount)
	}
}

// TestCreditSucceededDeposits_LedgerErrorLeavesSucceeded proves a
// transient ledger-svc failure leaves the deposit exactly as it was —
// still 'succeeded', ready to be retried on the next tick — rather than
// being marked failed or credited incorrectly.
func TestCreditSucceededDeposits_LedgerErrorLeavesSucceeded(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUIDForTest(t)
	intentID := "pi_" + randomUUIDForTest(t)
	depositID := insertDepositForReconcile(t, ctx, pool, accountID, 5000, "succeeded", &intentID)

	ledgerClient := &fakeLedgerClient{
		depositFunc: func(ctx context.Context, req *ledgerv1.DepositRequest) (*ledgerv1.DepositResponse, error) {
			return nil, errors.New("simulated ledger-svc outage")
		},
	}

	creditSucceededDeposits(ctx, pool, ledgerClient, time.Hour)

	var status string
	if err := pool.QueryRow(ctx, "SELECT status FROM deposits WHERE id = $1", depositID).Scan(&status); err != nil {
		t.Fatalf("query deposit: %v", err)
	}
	if status != "succeeded" {
		t.Errorf("status = %q, want %q (unchanged, ready to retry)", status, "succeeded")
	}
}

// TestCreditDeposit_LogsDivergenceAlertWhenStale proves the "most
// important invariant" check actually fires: a 'succeeded' deposit older
// than divergenceAfter produces the distinctly-tagged alert log line,
// regardless of what this attempt does next.
func TestCreditDeposit_LogsDivergenceAlertWhenStale(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUIDForTest(t)
	intentID := "pi_" + randomUUIDForTest(t)
	depositID := insertDepositForReconcile(t, ctx, pool, accountID, 5000, "succeeded", &intentID)
	ageDeposit(t, ctx, pool, depositID, 10*time.Minute)

	ledgerClient := &fakeLedgerClient{
		depositFunc: func(ctx context.Context, req *ledgerv1.DepositRequest) (*ledgerv1.DepositResponse, error) {
			return nil, errors.New("still down")
		},
	}

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(defaultLogOutput) })

	creditSucceededDeposits(ctx, pool, ledgerClient, time.Minute)

	if !bytes.Contains(logBuf.Bytes(), []byte(depositDivergenceLogPrefix)) {
		t.Errorf("expected log output to contain %q, got:\n%s", depositDivergenceLogPrefix, logBuf.String())
	}
	if !bytes.Contains(logBuf.Bytes(), []byte(depositID)) {
		t.Errorf("expected divergence log to mention deposit id %s, got:\n%s", depositID, logBuf.String())
	}
}

func TestReconcilePendingDepositsWithIntent_ResolvesToSucceeded(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUIDForTest(t)
	intentID := "pi_" + randomUUIDForTest(t)
	depositID := insertDepositForReconcile(t, ctx, pool, accountID, 5000, "pending", &intentID)
	ageDeposit(t, ctx, pool, depositID, time.Hour)

	paymentIntents := &fakePaymentIntentRetriever{
		retrieveFunc: func(ctx context.Context, id string, params *stripe.PaymentIntentRetrieveParams) (*stripe.PaymentIntent, error) {
			if id != intentID {
				t.Errorf("Retrieve id = %s, want %s", id, intentID)
			}
			return &stripe.PaymentIntent{ID: intentID, Status: stripe.PaymentIntentStatusSucceeded}, nil
		},
	}

	reconcilePendingDepositsWithIntent(ctx, pool, paymentIntents, time.Minute)

	var status string
	if err := pool.QueryRow(ctx, "SELECT status FROM deposits WHERE id = $1", depositID).Scan(&status); err != nil {
		t.Fatalf("query deposit: %v", err)
	}
	if status != "succeeded" {
		t.Errorf("status = %q, want %q", status, "succeeded")
	}
}

func TestReconcilePendingDepositsWithIntent_ResolvesToFailedOnCanceled(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUIDForTest(t)
	intentID := "pi_" + randomUUIDForTest(t)
	depositID := insertDepositForReconcile(t, ctx, pool, accountID, 5000, "pending", &intentID)
	ageDeposit(t, ctx, pool, depositID, time.Hour)

	paymentIntents := &fakePaymentIntentRetriever{
		retrieveFunc: func(ctx context.Context, id string, params *stripe.PaymentIntentRetrieveParams) (*stripe.PaymentIntent, error) {
			return &stripe.PaymentIntent{ID: intentID, Status: stripe.PaymentIntentStatusCanceled}, nil
		},
	}

	reconcilePendingDepositsWithIntent(ctx, pool, paymentIntents, time.Minute)

	var status string
	var failureReason *string
	if err := pool.QueryRow(ctx, "SELECT status, failure_reason FROM deposits WHERE id = $1", depositID).Scan(&status, &failureReason); err != nil {
		t.Fatalf("query deposit: %v", err)
	}
	if status != "failed" {
		t.Errorf("status = %q, want %q", status, "failed")
	}
	if failureReason == nil || *failureReason != "canceled" {
		t.Errorf("failure_reason = %v, want %q", failureReason, "canceled")
	}
}

func TestReconcilePendingDepositsWithIntent_StillInProgressLeftPending(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUIDForTest(t)
	intentID := "pi_" + randomUUIDForTest(t)
	depositID := insertDepositForReconcile(t, ctx, pool, accountID, 5000, "pending", &intentID)
	ageDeposit(t, ctx, pool, depositID, time.Hour)

	paymentIntents := &fakePaymentIntentRetriever{
		retrieveFunc: func(ctx context.Context, id string, params *stripe.PaymentIntentRetrieveParams) (*stripe.PaymentIntent, error) {
			return &stripe.PaymentIntent{ID: intentID, Status: stripe.PaymentIntentStatusRequiresAction}, nil
		},
	}

	reconcilePendingDepositsWithIntent(ctx, pool, paymentIntents, time.Minute)

	var status string
	if err := pool.QueryRow(ctx, "SELECT status FROM deposits WHERE id = $1", depositID).Scan(&status); err != nil {
		t.Fatalf("query deposit: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want %q (still in progress at Stripe, must not be resolved yet)", status, "pending")
	}
}

// TestReconcilePendingDepositsWithIntent_RespectsStaleness proves a
// recent 'pending' deposit with an intent is NOT polled yet — the
// staleness gate exists so a deposit mid-checkout isn't queried on every
// 30s tick, only once the webhook has had a fair chance to arrive.
func TestReconcilePendingDepositsWithIntent_RespectsStaleness(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUIDForTest(t)
	intentID := "pi_" + randomUUIDForTest(t)
	insertDepositForReconcile(t, ctx, pool, accountID, 5000, "pending", &intentID)
	// No ageDeposit call: updated_at is "now", well within the staleAfter window.

	paymentIntents := &fakePaymentIntentRetriever{
		retrieveFunc: func(ctx context.Context, id string, params *stripe.PaymentIntentRetrieveParams) (*stripe.PaymentIntent, error) {
			t.Fatal("Stripe should not be polled for a deposit that isn't stale yet")
			return nil, nil
		},
	}

	reconcilePendingDepositsWithIntent(ctx, pool, paymentIntents, time.Hour)
}

func TestReconcileOrphanedPendingDeposits_MarksFailed(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUIDForTest(t)
	depositID := insertDepositForReconcile(t, ctx, pool, accountID, 5000, "pending", nil)
	ageDeposit(t, ctx, pool, depositID, time.Hour)

	reconcileOrphanedPendingDeposits(ctx, pool, time.Minute)

	var status string
	var failureReason *string
	if err := pool.QueryRow(ctx, "SELECT status, failure_reason FROM deposits WHERE id = $1", depositID).Scan(&status, &failureReason); err != nil {
		t.Fatalf("query deposit: %v", err)
	}
	if status != "failed" {
		t.Errorf("status = %q, want %q", status, "failed")
	}
	if failureReason == nil || *failureReason != "abandoned_before_payment_intent" {
		t.Errorf("failure_reason = %v, want %q", failureReason, "abandoned_before_payment_intent")
	}
}

// TestReconcileOrphanedPendingDeposits_RespectsStaleness proves a
// freshly-created pending deposit with no intent yet (the normal,
// momentary state between createDeposit's INSERT and its Stripe call
// returning) is not prematurely marked failed.
func TestReconcileOrphanedPendingDeposits_RespectsStaleness(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUIDForTest(t)
	depositID := insertDepositForReconcile(t, ctx, pool, accountID, 5000, "pending", nil)
	// No ageDeposit call.

	reconcileOrphanedPendingDeposits(ctx, pool, time.Hour)

	var status string
	if err := pool.QueryRow(ctx, "SELECT status FROM deposits WHERE id = $1", depositID).Scan(&status); err != nil {
		t.Fatalf("query deposit: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want %q (not stale yet)", status, "pending")
	}
}

var defaultLogOutput = log.Writer()
