package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"
)

// stripeWebhookMaxBodyBytes caps how much of the request body this handler
// will read before giving up — this is a public, unauthenticated endpoint,
// so it must never buffer an attacker-controlled body of unbounded size
// into memory. Real Stripe event payloads are a few KB; this is generous
// headroom.
const stripeWebhookMaxBodyBytes = 1 << 20 // 1 MiB

// stripeWebhookHandler is registered at "POST /webhooks/stripe" — public
// and unauthenticated, since Stripe has no way to send our JWT. That is
// exactly why signature verification is not optional here: without it,
// anyone who finds this URL could claim any payment succeeded and get an
// account credited for free.
//
// Deliberately thin: verify signature, deduplicate, update one row,
// respond. Stripe times out and retries a webhook delivery that takes too
// long or returns a non-2xx status, so crediting the ledger — a slower,
// cross-service call — happens in a later step, not here.
func stripeWebhookHandler(pool *pgxpool.Pool, webhookSecret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Signature verification needs the exact bytes Stripe signed. No
		// body-parsing middleware sits in front of this handler, but the
		// ordering still matters enough to call out: nothing between here
		// and webhook.ConstructEvent below may decode-then-reencode this
		// payload, or the signature will stop matching.
		payload, err := io.ReadAll(io.LimitReader(r.Body, stripeWebhookMaxBodyBytes))
		if err != nil {
			log.Printf("transfers-svc: stripe webhook: failed to read body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		event, err := webhook.ConstructEvent(payload, r.Header.Get("Stripe-Signature"), webhookSecret)
		if err != nil {
			// Deliberately no response body and no further processing: an
			// invalid signature means this request cannot be trusted to
			// have come from Stripe at all.
			log.Printf("transfers-svc: stripe webhook: signature verification failed: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if err := processStripeEvent(r.Context(), pool, event); err != nil {
			if errors.Is(err, errDuplicateStripeEvent) {
				// A redelivery of an event already fully processed — not
				// an error from Stripe's point of view. Acknowledge it so
				// Stripe stops retrying, and do nothing else.
				w.WriteHeader(http.StatusOK)
				return
			}
			// processStripeEvent rolls its own transaction back on any
			// other error (see its doc comment), so event.ID was NOT
			// recorded as processed — Stripe's automatic retry gets a
			// genuine second attempt rather than being silently swallowed
			// by the duplicate-event path above.
			log.Printf("transfers-svc: stripe webhook: failed to process event %s (%s): %v", event.ID, event.Type, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
	}
}

// errDuplicateStripeEvent marks a redelivery of an event already recorded
// in processed_stripe_events — a normal, expected outcome of Stripe's
// at-least-once delivery, not a failure.
var errDuplicateStripeEvent = errors.New("stripe event already processed")

// processStripeEvent records event as processed and applies its effect on
// the matching deposits row, in one Postgres transaction. The two writes
// must share a transaction: processed_stripe_events.event_id is the
// dedup constraint's arbiter (see below), so if it were recorded as
// processed BEFORE the deposits row was actually updated, a transient
// failure between those two steps would return 500 (asking Stripe to
// retry) while permanently blocking that same retry from ever taking
// effect — the redelivery would just hit the "duplicate, ignore" path
// with the real update never having happened. Wrapping both in one
// transaction means a failure here rolls back the processed_stripe_events
// insert too, so a retried delivery is indistinguishable from the first
// attempt.
func processStripeEvent(ctx context.Context, pool *pgxpool.Pool, event stripe.Event) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// The unique constraint on processed_stripe_events.event_id is the
	// actual arbiter of "have I seen this event before" — not a
	// SELECT-then-INSERT in application code, which would have the same
	// race two concurrent deliveries of the same event could hit here.
	_, err = tx.Exec(ctx,
		"INSERT INTO processed_stripe_events (event_id, event_type) VALUES ($1, $2)",
		event.ID, string(event.Type),
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return errDuplicateStripeEvent
		}
		return fmt.Errorf("record processed stripe event %s: %w", event.ID, err)
	}

	if err := applyStripeEvent(ctx, tx, event); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// applyStripeEvent updates the deposits row event refers to. Unrecognized
// event types are treated as success (nothing to do) rather than an
// error: Stripe sends many event types this service has no opinion on,
// and treating an unknown type as a failure would make Stripe retry it
// forever for no reason.
func applyStripeEvent(ctx context.Context, tx pgx.Tx, event stripe.Event) error {
	switch event.Type {
	case stripe.EventTypePaymentIntentSucceeded:
		var intent stripe.PaymentIntent
		if err := json.Unmarshal(event.Data.Raw, &intent); err != nil {
			return fmt.Errorf("unmarshal %s payload: %w", event.Type, err)
		}
		return markDepositSucceeded(ctx, tx, intent.ID)

	case stripe.EventTypePaymentIntentPaymentFailed:
		var intent stripe.PaymentIntent
		if err := json.Unmarshal(event.Data.Raw, &intent); err != nil {
			return fmt.Errorf("unmarshal %s payload: %w", event.Type, err)
		}
		return markDepositFailed(ctx, tx, intent.ID, paymentIntentFailureReason(&intent))

	case stripe.EventTypeChargeRefunded:
		var charge stripe.Charge
		if err := json.Unmarshal(event.Data.Raw, &charge); err != nil {
			return fmt.Errorf("unmarshal %s payload: %w", event.Type, err)
		}
		if charge.PaymentIntent == nil || charge.PaymentIntent.ID == "" {
			log.Printf("transfers-svc: stripe webhook: %s event %s has no associated payment_intent, ignoring", event.Type, event.ID)
			return nil
		}
		return markDepositRefunded(ctx, tx, charge.PaymentIntent.ID)

	default:
		return nil
	}
}

// paymentIntentFailureReason extracts a short, storable reason from a
// failed PaymentIntent's last error — its Code when Stripe provided one
// (a stable short string like "card_declined"), falling back to the
// human-readable message, matching this codebase's existing convention of
// short failure_reason codes (see transfer.go's failureReasonX
// constants).
func paymentIntentFailureReason(intent *stripe.PaymentIntent) string {
	if intent.LastPaymentError == nil {
		return "unknown"
	}
	if intent.LastPaymentError.Code != "" {
		return string(intent.LastPaymentError.Code)
	}
	if intent.LastPaymentError.Msg != "" {
		return intent.LastPaymentError.Msg
	}
	return "unknown"
}

func markDepositSucceeded(ctx context.Context, tx pgx.Tx, stripePaymentIntentID string) error {
	tag, err := tx.Exec(ctx,
		"UPDATE deposits SET status = 'succeeded', updated_at = now() WHERE stripe_payment_intent_id = $1",
		stripePaymentIntentID,
	)
	if err != nil {
		return fmt.Errorf("mark deposit succeeded for payment_intent %s: %w", stripePaymentIntentID, err)
	}
	if tag.RowsAffected() == 0 {
		log.Printf("transfers-svc: stripe webhook: payment_intent %s has no matching deposit row", stripePaymentIntentID)
	}
	return nil
}

func markDepositFailed(ctx context.Context, tx pgx.Tx, stripePaymentIntentID, failureReason string) error {
	tag, err := tx.Exec(ctx,
		"UPDATE deposits SET status = 'failed', failure_reason = $1, updated_at = now() WHERE stripe_payment_intent_id = $2",
		failureReason, stripePaymentIntentID,
	)
	if err != nil {
		return fmt.Errorf("mark deposit failed for payment_intent %s: %w", stripePaymentIntentID, err)
	}
	if tag.RowsAffected() == 0 {
		log.Printf("transfers-svc: stripe webhook: payment_intent %s has no matching deposit row", stripePaymentIntentID)
	}
	return nil
}

// markDepositRefunded flags a deposit as refunded without changing its
// status: the deposits status vocabulary ('pending'/'succeeded'/'failed'/
// 'credited') has no dedicated 'refunded' value — building that out is
// explicitly deferred along with the rest of reversal handling (see
// README). Reusing failure_reason as a breadcrumb here, even though a
// refund isn't a "failure", mirrors how Transfer already lets
// failure_reason's meaning depend on context (transfer.go's doc comment
// on FailureReason), and gives a future reversal step a queryable signal
// (status = 'succeeded' AND failure_reason IS NOT NULL) without a schema
// change now.
func markDepositRefunded(ctx context.Context, tx pgx.Tx, stripePaymentIntentID string) error {
	tag, err := tx.Exec(ctx,
		"UPDATE deposits SET failure_reason = 'refunded', updated_at = now() WHERE stripe_payment_intent_id = $1",
		stripePaymentIntentID,
	)
	if err != nil {
		return fmt.Errorf("mark deposit refunded for payment_intent %s: %w", stripePaymentIntentID, err)
	}
	if tag.RowsAffected() == 0 {
		log.Printf("transfers-svc: stripe webhook: refunded payment_intent %s has no matching deposit row", stripePaymentIntentID)
	}
	return nil
}
