package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v86"

	accountsv1 "neobank/proto/gen/go/accounts/v1"
)

const (
	// depositMinAmount matches Stripe's own minimum charge amount for EUR
	// (currently €0.50) — Stripe would reject anything smaller itself, but
	// checking it here turns that into a clean 400 instead of a
	// pass-through Stripe API error.
	depositMinAmount int64 = 50
	// depositMaxAmount is a defensive ceiling against a fat-fingered amount
	// (e.g. three extra zeros turning a €50 top-up into €50,000) — not a
	// business or compliance limit, just a sanity check, set well above
	// any realistic single top-up for this MVP.
	depositMaxAmount int64 = 1_000_000 // €10,000.00
)

// Deposit is a deposits row: transfers-svc's own record of a Stripe-funded
// top-up attempt, mirroring how Transfer tracks a transfer's lifecycle
// separately from ledger-svc's entries log. status stays 'pending' through
// this file's entire flow — 'succeeded' and 'credited' are only ever set by
// the (future) webhook handler once Stripe actually confirms the charge.
type Deposit struct {
	ID                    string    `json:"id"`
	AccountID             string    `json:"account_id"`
	Amount                int64     `json:"amount"`
	Currency              string    `json:"currency"`
	Status                string    `json:"status"`
	StripePaymentIntentID *string   `json:"stripe_payment_intent_id,omitempty"`
	LedgerTransactionID   *string   `json:"ledger_transaction_id,omitempty"`
	FailureReason         *string   `json:"failure_reason,omitempty"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

const depositColumns = `id, account_id, amount, currency, status, stripe_payment_intent_id, ledger_transaction_id, failure_reason, created_at, updated_at`

type createDepositOutcome int

const (
	createDepositOK createDepositOutcome = iota
	createDepositInvalidAmount
	createDepositAccountNotActive
)

// stripePaymentIntentCreator is the one Stripe SDK method createDeposit
// needs, narrowed to an interface so tests can substitute a fake instead of
// making real Stripe API calls — same shape as fakeLedgerClient /
// fakeAccountsClient in transfer_test.go. stripeClient.V1PaymentIntents
// (wired up in main.go) satisfies this directly.
type stripePaymentIntentCreator interface {
	Create(ctx context.Context, params *stripe.PaymentIntentCreateParams) (*stripe.PaymentIntent, error)
}

// createDeposit inserts a pending deposits row, then creates a matching
// Stripe PaymentIntent and persists its id back onto the row.
//
// The two writes are deliberately not wrapped in one transaction — they
// can't be, since the Stripe API call happens in between and Stripe has no
// way to participate in a Postgres transaction. If the process dies
// between them, the result is a pending deposit with no
// stripe_payment_intent_id: safe, because no money has moved (Stripe never
// received a fully-formed request in most such failures, and even if it
// did, nothing has been confirmed yet) — it's simply an abandoned attempt,
// left for future reconciliation work to clean up rather than guessed at
// here.
//
// account_id is never taken from the request body — same reasoning as
// resolveSenderAccountID for transfers: a client must never be able to
// deposit into an account that isn't theirs.
func createDeposit(ctx context.Context, pool *pgxpool.Pool, accountsClient accountsv1.AccountsServiceClient, paymentIntents stripePaymentIntentCreator, accountID string, amount int64) (deposit Deposit, clientSecret string, outcome createDepositOutcome, err error) {
	if amount < depositMinAmount || amount > depositMaxAmount {
		return Deposit{}, "", createDepositInvalidAmount, nil
	}

	account, err := accountsClient.GetAccountByID(ctx, &accountsv1.GetAccountByIDRequest{AccountId: accountID})
	if err != nil {
		return Deposit{}, "", 0, fmt.Errorf("look up account: %w", err)
	}
	if account.GetStatus() != accountStatusActive {
		return Deposit{}, "", createDepositAccountNotActive, nil
	}

	var d Deposit
	err = pool.QueryRow(ctx,
		`INSERT INTO deposits (account_id, amount) VALUES ($1, $2) RETURNING `+depositColumns,
		accountID, amount,
	).Scan(&d.ID, &d.AccountID, &d.Amount, &d.Currency, &d.Status, &d.StripePaymentIntentID, &d.LedgerTransactionID, &d.FailureReason, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return Deposit{}, "", 0, fmt.Errorf("insert pending deposit: %w", err)
	}

	params := &stripe.PaymentIntentCreateParams{
		Amount:   stripe.Int64(d.Amount),
		Currency: stripe.String(d.Currency),
		// Metadata is what lets the future webhook handler tie an incoming
		// Stripe event back to this exact deposits row, since Stripe has
		// no notion of our own primary keys otherwise.
		Metadata: map[string]string{
			"deposit_id": d.ID,
			"account_id": d.AccountID,
		},
	}
	// Ties Stripe-side retry safety to this specific deposit attempt: a
	// retried call (network blip, our own process retrying) using the same
	// deposit_id returns the original PaymentIntent instead of creating a
	// second one on the same attempt.
	params.IdempotencyKey = stripe.String(d.ID)

	intent, err := paymentIntents.Create(ctx, params)
	if err != nil {
		// The pending row is left exactly as it is — see the doc comment
		// above on why that's safe.
		return Deposit{}, "", 0, fmt.Errorf("create stripe payment intent: %w", err)
	}

	err = pool.QueryRow(ctx,
		`UPDATE deposits SET stripe_payment_intent_id = $1, updated_at = now() WHERE id = $2 RETURNING `+depositColumns,
		intent.ID, d.ID,
	).Scan(&d.ID, &d.AccountID, &d.Amount, &d.Currency, &d.Status, &d.StripePaymentIntentID, &d.LedgerTransactionID, &d.FailureReason, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return Deposit{}, "", 0, fmt.Errorf("persist stripe_payment_intent_id: %w", err)
	}

	return d, intent.ClientSecret, createDepositOK, nil
}
