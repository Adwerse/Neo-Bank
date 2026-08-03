package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"

	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

// noReverseDepositExpectedLedgerClient is a fakeLedgerClient whose
// reverseDepositFunc fails the test if called — the default for every
// webhook test that isn't specifically exercising charge.refunded's
// credited-deposit reversal path, so an accidental/unwanted ledger call
// is caught immediately instead of silently doing nothing (a nil
// reverseDepositFunc would panic instead, which is a less clear failure).
func noReverseDepositExpectedLedgerClient(t *testing.T) *fakeLedgerClient {
	t.Helper()
	return &fakeLedgerClient{
		reverseDepositFunc: func(ctx context.Context, req *ledgerv1.DepositRequest) (*ledgerv1.DepositResponse, error) {
			t.Fatal("ReverseDeposit should not be called for this event")
			return nil, nil
		},
	}
}

const testWebhookSecret = "whsec_test_secret_not_a_real_stripe_value"

// insertDepositWithIntent inserts a deposits row already carrying
// stripePaymentIntentID, as createDeposit would leave it after a
// successful PaymentIntent creation — the state a webhook event actually
// arrives to find. Registers cleanup of the row.
func insertDepositWithIntent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID, stripePaymentIntentID string, amount int64) string {
	t.Helper()
	var depositID string
	err := pool.QueryRow(ctx,
		"INSERT INTO deposits (account_id, amount, stripe_payment_intent_id) VALUES ($1, $2, $3) RETURNING id",
		accountID, amount, stripePaymentIntentID,
	).Scan(&depositID)
	if err != nil {
		t.Fatalf("insert deposit with stripe_payment_intent_id=%s: %v", stripePaymentIntentID, err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM deposits WHERE id = $1", depositID); err != nil {
			t.Logf("cleanup: delete deposit id=%s: %v", depositID, err)
		}
	})
	return depositID
}

// cleanupProcessedStripeEvent removes a processed_stripe_events row so
// repeated test runs don't collide on its event_id primary key.
func cleanupProcessedStripeEvent(t *testing.T, pool *pgxpool.Pool, eventID string) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM processed_stripe_events WHERE event_id = $1", eventID); err != nil {
			t.Logf("cleanup: delete processed_stripe_events event_id=%s: %v", eventID, err)
		}
	})
}

// stripeEventJSON builds a minimal Stripe event payload: just enough
// structure (id, api_version, type, data.object) for webhook.ConstructEvent
// and applyStripeEvent to do real work against — Stripe's own event
// payloads are a strict superset of these fields. api_version must match
// stripe.APIVersion or ConstructEvent rejects the event outright (a real
// webhook endpoint configured for a different API version would hit this
// too — it's stripe-go's own validation, not something this codebase
// added).
func stripeEventJSON(eventID, eventType, dataObjectJSON string) []byte {
	return []byte(fmt.Sprintf(`{"id":%q,"object":"event","api_version":%q,"type":%q,"data":{"object":%s}}`, eventID, stripe.APIVersion, eventType, dataObjectJSON))
}

// sendSignedWebhook signs payload with secret exactly as Stripe would
// (via webhook.GenerateTestSignedPayload — real HMAC computation, not a
// mock) and delivers it to stripeWebhookHandler, which always verifies
// against testWebhookSecret.
func sendSignedWebhook(t *testing.T, pool *pgxpool.Pool, ledgerClient ledgerv1.LedgerServiceClient, payload []byte, secret string) *httptest.ResponseRecorder {
	t.Helper()
	signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: secret})
	req := httptest.NewRequest(http.MethodPost, "/webhooks/stripe", bytes.NewReader(signed.Payload))
	req.Header.Set("Stripe-Signature", signed.Header)
	rec := httptest.NewRecorder()
	stripeWebhookHandler(pool, ledgerClient, testWebhookSecret)(rec, req)
	return rec
}

func TestStripeWebhookHandler_PaymentIntentSucceeded(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUIDForTest(t)
	intentID := "pi_" + randomUUIDForTest(t)
	eventID := "evt_" + randomUUIDForTest(t)
	depositID := insertDepositWithIntent(t, ctx, pool, accountID, intentID, 5000)
	cleanupProcessedStripeEvent(t, pool, eventID)

	payload := stripeEventJSON(eventID, "payment_intent.succeeded", fmt.Sprintf(`{"id":%q,"object":"payment_intent"}`, intentID))
	rec := sendSignedWebhook(t, pool, noReverseDepositExpectedLedgerClient(t), payload, testWebhookSecret)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var status string
	if err := pool.QueryRow(ctx, "SELECT status FROM deposits WHERE id = $1", depositID).Scan(&status); err != nil {
		t.Fatalf("query deposit status: %v", err)
	}
	if status != "succeeded" {
		t.Errorf("deposit status = %q, want %q", status, "succeeded")
	}

	var eventCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM processed_stripe_events WHERE event_id = $1", eventID).Scan(&eventCount); err != nil {
		t.Fatalf("query processed_stripe_events: %v", err)
	}
	if eventCount != 1 {
		t.Errorf("processed_stripe_events rows for event_id=%s = %d, want 1", eventID, eventCount)
	}
}

func TestStripeWebhookHandler_PaymentIntentPaymentFailed(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUIDForTest(t)
	intentID := "pi_" + randomUUIDForTest(t)
	eventID := "evt_" + randomUUIDForTest(t)
	depositID := insertDepositWithIntent(t, ctx, pool, accountID, intentID, 5000)
	cleanupProcessedStripeEvent(t, pool, eventID)

	dataObject := fmt.Sprintf(`{"id":%q,"object":"payment_intent","last_payment_error":{"code":"card_declined","message":"Your card was declined."}}`, intentID)
	payload := stripeEventJSON(eventID, "payment_intent.payment_failed", dataObject)
	rec := sendSignedWebhook(t, pool, noReverseDepositExpectedLedgerClient(t), payload, testWebhookSecret)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var status string
	var failureReason *string
	if err := pool.QueryRow(ctx, "SELECT status, failure_reason FROM deposits WHERE id = $1", depositID).Scan(&status, &failureReason); err != nil {
		t.Fatalf("query deposit: %v", err)
	}
	if status != "failed" {
		t.Errorf("deposit status = %q, want %q", status, "failed")
	}
	if failureReason == nil || *failureReason != "card_declined" {
		t.Errorf("deposit failure_reason = %v, want %q", failureReason, "card_declined")
	}
}

// TestStripeWebhookHandler_ChargeRefunded_CreditedDeposit is the core
// refund case: a deposit that was already credited (money moved into the
// user's ledger balance) gets refunded in Stripe, so ledger-svc must be
// asked to reverse it (account -> genesis), and the deposit's status
// moves to 'refunded'.
func TestStripeWebhookHandler_ChargeRefunded_CreditedDeposit(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUIDForTest(t)
	intentID := "pi_" + randomUUIDForTest(t)
	eventID := "evt_" + randomUUIDForTest(t)
	depositID := insertDepositWithIntent(t, ctx, pool, accountID, intentID, 5000)
	cleanupProcessedStripeEvent(t, pool, eventID)
	if _, err := pool.Exec(ctx, "UPDATE deposits SET status = 'credited' WHERE id = $1", depositID); err != nil {
		t.Fatalf("precondition: mark deposit credited: %v", err)
	}

	wantReversalTxnID := randomUUIDForTest(t)
	var gotReference string
	ledgerClient := &fakeLedgerClient{
		reverseDepositFunc: func(ctx context.Context, req *ledgerv1.DepositRequest) (*ledgerv1.DepositResponse, error) {
			if req.GetAccountId() != accountID || req.GetAmount() != 5000 {
				t.Errorf("ReverseDeposit request = %+v, want account=%s amount=5000", req, accountID)
			}
			if req.GetReference() == depositID {
				t.Error("ReverseDeposit reference must not equal the deposit's own id — it would collide with the original credit's reference in ledger-svc's idempotency check")
			}
			gotReference = req.GetReference()
			return &ledgerv1.DepositResponse{TransactionId: wantReversalTxnID}, nil
		},
	}

	dataObject := fmt.Sprintf(`{"id":"ch_%s","object":"charge","payment_intent":%q}`, randomUUIDForTest(t), intentID)
	payload := stripeEventJSON(eventID, "charge.refunded", dataObject)
	rec := sendSignedWebhook(t, pool, ledgerClient, payload, testWebhookSecret)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if gotReference == "" {
		t.Fatal("ReverseDeposit was never called")
	}

	var status string
	var failureReason *string
	if err := pool.QueryRow(ctx, "SELECT status, failure_reason FROM deposits WHERE id = $1", depositID).Scan(&status, &failureReason); err != nil {
		t.Fatalf("query deposit: %v", err)
	}
	if status != "refunded" {
		t.Errorf("deposit status = %q, want %q", status, "refunded")
	}

	// Redelivery of the same event: by now the deposit is no longer
	// 'credited', so this must NOT call ReverseDeposit again.
	second := sendSignedWebhook(t, pool, noReverseDepositExpectedLedgerClient(t), payload, testWebhookSecret)
	if second.Code != http.StatusOK {
		t.Fatalf("redelivery: status = %d, want 200; body=%s", second.Code, second.Body.String())
	}
}

// TestStripeWebhookHandler_ChargeRefunded_SucceededNotYetCreditedDeposit
// covers the case processChargeRefundedEvent exists specifically to
// handle correctly: Stripe refunded the charge before this service got
// around to crediting it. There is no ledger entry to reverse, so the
// deposit is marked failed instead of ever being credited.
func TestStripeWebhookHandler_ChargeRefunded_SucceededNotYetCreditedDeposit(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUIDForTest(t)
	intentID := "pi_" + randomUUIDForTest(t)
	eventID := "evt_" + randomUUIDForTest(t)
	depositID := insertDepositWithIntent(t, ctx, pool, accountID, intentID, 5000)
	cleanupProcessedStripeEvent(t, pool, eventID)
	if _, err := pool.Exec(ctx, "UPDATE deposits SET status = 'succeeded' WHERE id = $1", depositID); err != nil {
		t.Fatalf("precondition: mark deposit succeeded: %v", err)
	}

	dataObject := fmt.Sprintf(`{"id":"ch_%s","object":"charge","payment_intent":%q}`, randomUUIDForTest(t), intentID)
	payload := stripeEventJSON(eventID, "charge.refunded", dataObject)
	rec := sendSignedWebhook(t, pool, noReverseDepositExpectedLedgerClient(t), payload, testWebhookSecret)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var status string
	var failureReason *string
	if err := pool.QueryRow(ctx, "SELECT status, failure_reason FROM deposits WHERE id = $1", depositID).Scan(&status, &failureReason); err != nil {
		t.Fatalf("query deposit: %v", err)
	}
	if status != "failed" {
		t.Errorf("deposit status = %q, want %q", status, "failed")
	}
	if failureReason == nil || *failureReason != "refunded_before_credit" {
		t.Errorf("deposit failure_reason = %v, want %q", failureReason, "refunded_before_credit")
	}
}

// TestStripeWebhookHandler_ChargeRefunded_PendingDepositIsIgnored proves
// a refund on a deposit that never even reached 'succeeded' (nothing was
// ever confirmed, let alone credited) is a no-op besides being recorded
// as processed — there is nothing to reverse or fail.
func TestStripeWebhookHandler_ChargeRefunded_PendingDepositIsIgnored(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUIDForTest(t)
	intentID := "pi_" + randomUUIDForTest(t)
	eventID := "evt_" + randomUUIDForTest(t)
	depositID := insertDepositWithIntent(t, ctx, pool, accountID, intentID, 5000)
	cleanupProcessedStripeEvent(t, pool, eventID)

	dataObject := fmt.Sprintf(`{"id":"ch_%s","object":"charge","payment_intent":%q}`, randomUUIDForTest(t), intentID)
	payload := stripeEventJSON(eventID, "charge.refunded", dataObject)
	rec := sendSignedWebhook(t, pool, noReverseDepositExpectedLedgerClient(t), payload, testWebhookSecret)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var status string
	if err := pool.QueryRow(ctx, "SELECT status FROM deposits WHERE id = $1", depositID).Scan(&status); err != nil {
		t.Fatalf("query deposit: %v", err)
	}
	if status != "pending" {
		t.Errorf("deposit status = %q, want %q (unchanged)", status, "pending")
	}
}

func TestStripeWebhookHandler_UnknownEventTypeIsIgnored(t *testing.T) {
	pool := newTestPool(t)
	eventID := "evt_" + randomUUIDForTest(t)
	cleanupProcessedStripeEvent(t, pool, eventID)

	payload := stripeEventJSON(eventID, "customer.created", `{"id":"cus_irrelevant"}`)
	rec := sendSignedWebhook(t, pool, noReverseDepositExpectedLedgerClient(t), payload, testWebhookSecret)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unknown event types must be acknowledged, not retried forever); body=%s", rec.Code, rec.Body.String())
	}
}

// TestStripeWebhookHandler_InvalidSignature proves a request signed with
// the wrong secret (standing in for a forged or corrupted signature) is
// rejected with 400 and never reaches event processing — no
// processed_stripe_events row is written.
func TestStripeWebhookHandler_InvalidSignature(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	eventID := "evt_" + randomUUIDForTest(t)
	cleanupProcessedStripeEvent(t, pool, eventID)

	payload := stripeEventJSON(eventID, "payment_intent.succeeded", `{"id":"pi_irrelevant","object":"payment_intent"}`)
	rec := sendSignedWebhook(t, pool, noReverseDepositExpectedLedgerClient(t), payload, "whsec_a_completely_different_secret")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}

	var eventCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM processed_stripe_events WHERE event_id = $1", eventID).Scan(&eventCount); err != nil {
		t.Fatalf("query processed_stripe_events: %v", err)
	}
	if eventCount != 0 {
		t.Errorf("processed_stripe_events rows for a forged-signature event = %d, want 0 — it must never be recorded as processed", eventCount)
	}
}

// TestStripeWebhookHandler_DuplicateDeliveryIsNotReprocessed proves
// Stripe's at-least-once redelivery of the same event_id is acknowledged
// (200) but only actually applied once: a second delivery must not
// re-run applyStripeEvent, and processed_stripe_events must end up with
// exactly one row for that event_id.
func TestStripeWebhookHandler_DuplicateDeliveryIsNotReprocessed(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUIDForTest(t)
	intentID := "pi_" + randomUUIDForTest(t)
	eventID := "evt_" + randomUUIDForTest(t)
	depositID := insertDepositWithIntent(t, ctx, pool, accountID, intentID, 5000)
	cleanupProcessedStripeEvent(t, pool, eventID)

	payload := stripeEventJSON(eventID, "payment_intent.succeeded", fmt.Sprintf(`{"id":%q,"object":"payment_intent"}`, intentID))

	first := sendSignedWebhook(t, pool, noReverseDepositExpectedLedgerClient(t), payload, testWebhookSecret)
	if first.Code != http.StatusOK {
		t.Fatalf("first delivery: status = %d, want 200; body=%s", first.Code, first.Body.String())
	}

	var updatedAtAfterFirst any
	if err := pool.QueryRow(ctx, "SELECT updated_at FROM deposits WHERE id = $1", depositID).Scan(&updatedAtAfterFirst); err != nil {
		t.Fatalf("query updated_at after first delivery: %v", err)
	}

	// A second, real redelivery of the exact same signed bytes — exactly
	// what Stripe sends on retry.
	second := sendSignedWebhook(t, pool, noReverseDepositExpectedLedgerClient(t), payload, testWebhookSecret)
	if second.Code != http.StatusOK {
		t.Fatalf("second delivery: status = %d, want 200; body=%s", second.Code, second.Body.String())
	}

	var status string
	var updatedAtAfterSecond any
	if err := pool.QueryRow(ctx, "SELECT status, updated_at FROM deposits WHERE id = $1", depositID).Scan(&status, &updatedAtAfterSecond); err != nil {
		t.Fatalf("query deposit after second delivery: %v", err)
	}
	if status != "succeeded" {
		t.Errorf("deposit status = %q, want %q", status, "succeeded")
	}
	if fmt.Sprint(updatedAtAfterSecond) != fmt.Sprint(updatedAtAfterFirst) {
		t.Errorf("deposit updated_at changed on a duplicate delivery (%v -> %v) — it was reprocessed, not deduplicated", updatedAtAfterFirst, updatedAtAfterSecond)
	}

	var eventCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM processed_stripe_events WHERE event_id = $1", eventID).Scan(&eventCount); err != nil {
		t.Fatalf("query processed_stripe_events: %v", err)
	}
	if eventCount != 1 {
		t.Errorf("processed_stripe_events rows for event_id=%s after 2 deliveries = %d, want 1", eventID, eventCount)
	}
}

// TestProcessStripeEvent_ProcessingFailureDoesNotRecordEvent proves the
// transactional wrapping documented on processStripeEvent: if applying
// the event fails (here, a malformed data.object that can't unmarshal),
// the processed_stripe_events insert is rolled back too, so a genuine
// retry from Stripe would be treated as a fresh attempt rather than
// silently swallowed as an already-seen duplicate.
func TestProcessStripeEvent_ProcessingFailureDoesNotRecordEvent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	eventID := "evt_" + randomUUIDForTest(t)
	cleanupProcessedStripeEvent(t, pool, eventID)

	// data.object is a structurally valid JSON object (so it passes
	// ConstructEvent's own generic parsing) but gives "amount" a string
	// value where stripe.PaymentIntent declares int64 — json.Unmarshal
	// into stripe.PaymentIntent fails on that type mismatch, forcing
	// applyStripeEvent to return an error after the processed_stripe_events
	// insert already ran.
	payload := stripeEventJSON(eventID, "payment_intent.succeeded", `{"id":"pi_malformed","object":"payment_intent","amount":"not-a-number"}`)
	rec := sendSignedWebhook(t, pool, noReverseDepositExpectedLedgerClient(t), payload, testWebhookSecret)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (so Stripe retries)", rec.Code)
	}

	var eventCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM processed_stripe_events WHERE event_id = $1", eventID).Scan(&eventCount); err != nil {
		t.Fatalf("query processed_stripe_events: %v", err)
	}
	if eventCount != 0 {
		t.Errorf("processed_stripe_events rows after a failed processing attempt = %d, want 0 — the transaction must have rolled back", eventCount)
	}
}
