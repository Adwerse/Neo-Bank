package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v86"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"neobank/pkg/outbox"
	accountsv1 "neobank/proto/gen/go/accounts/v1"
	eventsv1 "neobank/proto/gen/go/events/v1"
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
// separately from ledger-svc's entries log. Status moves pending ->
// succeeded (webhook.go, Stripe confirmed the charge) -> credited
// (deposit_reconcile.go, ledger-svc actually posted the entry) -> refunded
// (webhook.go, only reachable from credited). failed is reachable from
// pending (Stripe declined, or the deposit was abandoned before ever
// getting a stripe_payment_intent_id) or from succeeded (refunded before
// this service got around to crediting it — see markDepositRefunded).
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

// rowScanner is satisfied by both pgx.Row (QueryRow) and pgx.Rows
// (Query, inside a rows.Next() loop) — narrowed to just what scanDeposit
// needs so one scan function serves both single-row and list queries
// against deposits.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanDeposit(row rowScanner, d *Deposit) error {
	return row.Scan(&d.ID, &d.AccountID, &d.Amount, &d.Currency, &d.Status, &d.StripePaymentIntentID, &d.LedgerTransactionID, &d.FailureReason, &d.CreatedAt, &d.UpdatedAt)
}

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
	err = scanDeposit(pool.QueryRow(ctx,
		`INSERT INTO deposits (account_id, amount) VALUES ($1, $2) RETURNING `+depositColumns,
		accountID, amount,
	), &d)
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

	err = scanDeposit(pool.QueryRow(ctx,
		`UPDATE deposits SET stripe_payment_intent_id = $1, updated_at = now() WHERE id = $2 RETURNING `+depositColumns,
		intent.ID, d.ID,
	), &d)
	if err != nil {
		return Deposit{}, "", 0, fmt.Errorf("persist stripe_payment_intent_id: %w", err)
	}

	return d, intent.ClientSecret, createDepositOK, nil
}

// markDepositCreditedIfSucceeded moves a deposit from 'succeeded' to
// 'credited' and records ledgerTransactionID, publishing DepositCredited
// to the outbox in the same transaction — same pattern as
// markTransferCompletedIfPending (transfer.go). The AND status =
// 'succeeded' guard means a concurrent caller that already credited this
// same deposit (a second transfers-svc replica's crediting worker
// overlapping this one's tick, in a horizontally-scaled deployment) loses
// the race harmlessly: RowsAffected() is 0, nothing is double-published.
// That race is otherwise real because ledger-svc's Deposit call this
// function's caller already made is itself idempotent and safe to have
// run twice — it's specifically the LOCAL bookkeeping (this row's status,
// the outbox event) that must not double-fire.
func markDepositCreditedIfSucceeded(ctx context.Context, pool *pgxpool.Pool, depositID, ledgerTransactionID string) (credited bool, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("mark deposit credited: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var d Deposit
	err = scanDeposit(tx.QueryRow(ctx,
		`UPDATE deposits SET status = 'credited', ledger_transaction_id = $1, updated_at = now()
		 WHERE id = $2 AND status = 'succeeded'
		 RETURNING `+depositColumns,
		ledgerTransactionID, depositID,
	), &d)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("mark deposit credited: %w", err)
	}

	eventID, err := outbox.GenerateEventID()
	if err != nil {
		return false, fmt.Errorf("mark deposit credited: generate event id: %w", err)
	}
	payload, err := proto.Marshal(&eventsv1.DepositCredited{
		EventId:             eventID,
		DepositId:           d.ID,
		AccountId:           d.AccountID,
		Amount:              d.Amount,
		LedgerTransactionId: ledgerTransactionID,
		OccurredAt:          timestamppb.New(time.Now()),
	})
	if err != nil {
		return false, fmt.Errorf("mark deposit credited: marshal event: %w", err)
	}
	if err := outbox.InsertEvent(ctx, tx, outboxTable, eventID, "DepositCredited", d.AccountID, payload); err != nil {
		return false, fmt.Errorf("mark deposit credited: insert outbox event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("mark deposit credited: commit tx: %w", err)
	}
	return true, nil
}

// getSucceededDeposits returns every deposit currently in 'succeeded' —
// Stripe confirmed the charge, ledger-svc has not yet been credited. This
// is deposit_reconcile.go's primary worklist: every one of these is
// eligible for a credit attempt on every tick, not just ones stale beyond
// some threshold — see runReconciliationWorker's doc comment for why
// crediting isn't gated by staleness the way transfer reconciliation is.
func getSucceededDeposits(ctx context.Context, pool *pgxpool.Pool) ([]Deposit, error) {
	rows, err := pool.Query(ctx, `SELECT `+depositColumns+` FROM deposits WHERE status = 'succeeded' ORDER BY updated_at`)
	if err != nil {
		return nil, fmt.Errorf("query succeeded deposits: %w", err)
	}
	defer rows.Close()

	var deposits []Deposit
	for rows.Next() {
		var d Deposit
		if err := scanDeposit(rows, &d); err != nil {
			return nil, fmt.Errorf("scan deposit: %w", err)
		}
		deposits = append(deposits, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate succeeded deposits: %w", err)
	}
	return deposits, nil
}

// getPendingDepositsWithIntentBefore returns 'pending' deposits that DO
// have a stripe_payment_intent_id (so createDeposit's Stripe call
// succeeded) whose updated_at is older than before — the second
// reconciliation category: a webhook that likely never arrived (or was
// lost), worth resolving by asking Stripe directly instead of waiting
// forever.
func getPendingDepositsWithIntentBefore(ctx context.Context, pool *pgxpool.Pool, before time.Time) ([]Deposit, error) {
	rows, err := pool.Query(ctx,
		`SELECT `+depositColumns+` FROM deposits
		 WHERE status = 'pending' AND stripe_payment_intent_id IS NOT NULL AND updated_at < $1
		 ORDER BY updated_at`,
		before,
	)
	if err != nil {
		return nil, fmt.Errorf("query pending deposits with intent: %w", err)
	}
	defer rows.Close()

	var deposits []Deposit
	for rows.Next() {
		var d Deposit
		if err := scanDeposit(rows, &d); err != nil {
			return nil, fmt.Errorf("scan deposit: %w", err)
		}
		deposits = append(deposits, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending deposits with intent: %w", err)
	}
	return deposits, nil
}

// getOrphanedPendingDeposits returns 'pending' deposits with NO
// stripe_payment_intent_id whose updated_at is older than before — the
// third reconciliation category: createDeposit crashed (or its Stripe
// call failed) between inserting the row and persisting the intent id.
// No money ever moved for these (Stripe either never saw the request or
// never confirmed it), so there is nothing to reconcile against Stripe —
// they're simply marked failed.
func getOrphanedPendingDeposits(ctx context.Context, pool *pgxpool.Pool, before time.Time) ([]Deposit, error) {
	rows, err := pool.Query(ctx,
		`SELECT `+depositColumns+` FROM deposits
		 WHERE status = 'pending' AND stripe_payment_intent_id IS NULL AND updated_at < $1
		 ORDER BY updated_at`,
		before,
	)
	if err != nil {
		return nil, fmt.Errorf("query orphaned pending deposits: %w", err)
	}
	defer rows.Close()

	var deposits []Deposit
	for rows.Next() {
		var d Deposit
		if err := scanDeposit(rows, &d); err != nil {
			return nil, fmt.Errorf("scan deposit: %w", err)
		}
		deposits = append(deposits, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate orphaned pending deposits: %w", err)
	}
	return deposits, nil
}

// markDepositFailedIfPending mirrors markTransferFailedIfPending's guard:
// AND status = 'pending' so a concurrent write (createDeposit finishing
// late, or a webhook arriving between this row being read and written)
// wins over this call's stale view rather than being clobbered by it.
func markDepositFailedIfPending(ctx context.Context, pool *pgxpool.Pool, depositID, failureReason string) (marked bool, err error) {
	tag, err := pool.Exec(ctx,
		`UPDATE deposits SET status = 'failed', failure_reason = $1, updated_at = now()
		 WHERE id = $2 AND status = 'pending'`,
		failureReason, depositID,
	)
	if err != nil {
		return false, fmt.Errorf("mark deposit failed if pending: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// markDepositSucceededIfPending is deposit_reconcile.go's write path for
// category 2 (a 'pending' deposit whose Stripe PaymentIntent turns out to
// already be succeeded, discovered by polling rather than a webhook).
// Guarded the same way markDepositFailedIfPending is, and for the same
// reason: a real webhook could arrive concurrently with this poll and
// must win if it gets there first.
func markDepositSucceededIfPending(ctx context.Context, pool *pgxpool.Pool, stripePaymentIntentID string) (marked bool, err error) {
	tag, err := pool.Exec(ctx,
		`UPDATE deposits SET status = 'succeeded', updated_at = now()
		 WHERE stripe_payment_intent_id = $1 AND status = 'pending'`,
		stripePaymentIntentID,
	)
	if err != nil {
		return false, fmt.Errorf("mark deposit succeeded if pending: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// markDepositFailedIfOrphaned is deposit_reconcile.go's write path for
// category 3. Guarded more tightly than markDepositFailedIfPending — AND
// stripe_payment_intent_id IS NULL, not just AND status = 'pending' —
// because the row this targets is specifically one createDeposit
// (deposit.go) never finished setting up: if createDeposit's own slow,
// still-in-flight UPDATE finally lands moments after
// getOrphanedPendingDeposits read this row (attaching a real
// stripe_payment_intent_id, status still 'pending'), that row has become
// a legitimate category-2 case and must not be marked failed out from
// under it.
func markDepositFailedIfOrphaned(ctx context.Context, pool *pgxpool.Pool, depositID, failureReason string) (marked bool, err error) {
	tag, err := pool.Exec(ctx,
		`UPDATE deposits SET status = 'failed', failure_reason = $1, updated_at = now()
		 WHERE id = $2 AND status = 'pending' AND stripe_payment_intent_id IS NULL`,
		failureReason, depositID,
	)
	if err != nil {
		return false, fmt.Errorf("mark deposit failed if orphaned: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// getDepositByStripePaymentIntentID looks up a deposit by the Stripe
// PaymentIntent id — the same lookup key webhook.go's markDepositSucceeded/
// markDepositFailed update by directly, but here as a read: refund
// handling needs to know the deposit's CURRENT status before deciding
// whether there's anything to reverse.
func getDepositByStripePaymentIntentID(ctx context.Context, pool *pgxpool.Pool, stripePaymentIntentID string) (Deposit, bool, error) {
	var d Deposit
	err := scanDeposit(pool.QueryRow(ctx,
		`SELECT `+depositColumns+` FROM deposits WHERE stripe_payment_intent_id = $1`,
		stripePaymentIntentID,
	), &d)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deposit{}, false, nil
	}
	if err != nil {
		return Deposit{}, false, fmt.Errorf("get deposit by stripe_payment_intent_id: %w", err)
	}
	return d, true, nil
}
