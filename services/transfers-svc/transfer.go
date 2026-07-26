package main

import (
	"context"
	crand "crypto/rand"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountsv1 "neobank/proto/gen/go/accounts/v1"
	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

const (
	accountStatusActive = "active"
	accountStatusClosed = "closed"
)

const ledgerCallTimeout = 5 * time.Second

const (
	failureReasonInsufficientFunds = "insufficient_funds"
	failureReasonAccountNotFound   = "account_not_found"
	failureReasonInvalidAmount     = "invalid_amount"
	failureReasonLedgerInternal    = "ledger_internal_error"
)

// Transfer is a transfers row: transfers-svc's own record of a transfer
// request's lifecycle (pending -> completed/failed), kept separate from
// ledger-svc's entries log because there's a gap between "client asked to
// transfer" and "ledger recorded the entry" where transfers-svc could crash
// or the network could drop the response — the idempotency_key attaches to
// this record, not to a ledger entry.
type Transfer struct {
	ID                  string    `json:"id"`
	IdempotencyKey      string    `json:"idempotency_key"`
	SenderAccountID     string    `json:"sender_account_id"`
	RecipientAccountID  string    `json:"recipient_account_id"`
	Amount              int64     `json:"amount"`
	Status              string    `json:"status"`
	FailureReason       *string   `json:"failure_reason,omitempty"`
	LedgerTransactionID *string   `json:"ledger_transaction_id,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type createTransferOutcome int

const (
	createTransferOK createTransferOutcome = iota
	createTransferInvalidAmount
	createTransferRecipientNotFound
	createTransferSelfTransfer
	createTransferRecipientClosed
	createTransferSenderNotActive
)

// createTransfer validates a transfer request and, on success, inserts a
// pending transfers row. It never moves any money: ledger-svc's atomic
// ExecuteTransfer (wired in a later sprint) is the only place a balance is
// actually checked or changed — checking it here would race that later
// debit, since the balance could change between this check and the debit.
//
// idempotencyKey is caller-supplied purely to satisfy the transfers table's
// NOT NULL UNIQUE column — there is no dedupe-on-existing-key logic yet
// (that's a later sprint), so a retried request with a fresh key today
// simply creates a second row.
func createTransfer(ctx context.Context, pool *pgxpool.Pool, accountsClient accountsv1.AccountsServiceClient, idempotencyKey, senderAccountID, recipientAccountNumber string, amount int64) (Transfer, createTransferOutcome, error) {
	if amount <= 0 {
		return Transfer{}, createTransferInvalidAmount, nil
	}

	recipient, err := accountsClient.ResolveAccountByNumber(ctx, &accountsv1.ResolveAccountByNumberRequest{AccountNumber: recipientAccountNumber})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return Transfer{}, createTransferRecipientNotFound, nil
		}
		return Transfer{}, 0, fmt.Errorf("resolve recipient account: %w", err)
	}
	if recipient.GetAccountId() == senderAccountID {
		return Transfer{}, createTransferSelfTransfer, nil
	}
	if recipient.GetStatus() == accountStatusClosed {
		return Transfer{}, createTransferRecipientClosed, nil
	}

	sender, err := accountsClient.GetAccountByID(ctx, &accountsv1.GetAccountByIDRequest{AccountId: senderAccountID})
	if err != nil {
		return Transfer{}, 0, fmt.Errorf("look up sender account: %w", err)
	}
	if sender.GetStatus() != accountStatusActive {
		return Transfer{}, createTransferSenderNotActive, nil
	}

	var t Transfer
	err = pool.QueryRow(ctx,
		`INSERT INTO transfers (idempotency_key, sender_account_id, recipient_account_id, amount)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, idempotency_key, sender_account_id, recipient_account_id, amount, status, failure_reason, ledger_transaction_id, created_at, updated_at`,
		idempotencyKey, senderAccountID, recipient.GetAccountId(), amount,
	).Scan(&t.ID, &t.IdempotencyKey, &t.SenderAccountID, &t.RecipientAccountID, &t.Amount, &t.Status, &t.FailureReason, &t.LedgerTransactionID, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return Transfer{}, 0, fmt.Errorf("insert pending transfer: %w", err)
	}
	return t, createTransferOK, nil
}

type settlementOutcome int

const (
	settlementCompleted settlementOutcome = iota
	settlementFailed
	settlementUncertain
)

// settleTransfer calls ledger-svc.ExecuteTransfer for an already-created
// pending transfer and updates the row to a DEFINITE outcome (completed or
// failed) whenever ledger-svc actually answered.
//
// ledger-svc's ExecuteTransfer (services/ledger-svc/ledger.go) wraps its
// work in a single Postgres transaction that either fully commits or fully
// rolls back before any gRPC response is sent — so any well-formed response
// (success, or one of its own explicit status.Error(...) codes below) tells
// us for certain whether the money moved. codes.Unavailable,
// codes.DeadlineExceeded, and codes.Unknown are different in kind: ledger-svc
// itself never returns them — they arise only from the transport layer
// (couldn't reach it, or gave up waiting) — meaning we genuinely don't know
// whether the request ever reached ledger-svc, or reached it and committed
// but the response was lost on the way back. Marking the transfer failed
// there could contradict a real debit; marking it completed without a
// transaction_id would be a lie. So it stays pending, unchanged. See
// README's "Перевод денег через ledger" section for the full boundary this
// leaves — real resolution needs reconciliation against ledger-svc (e.g. via
// GetHistory, or a request id ledger-svc doesn't yet accept), out of scope
// here.
func settleTransfer(ctx context.Context, pool *pgxpool.Pool, ledgerClient ledgerv1.LedgerServiceClient, transfer Transfer) (Transfer, settlementOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, ledgerCallTimeout)
	defer cancel()

	resp, err := ledgerClient.ExecuteTransfer(ctx, &ledgerv1.ExecuteTransferRequest{
		FromAccountId: transfer.SenderAccountID,
		ToAccountId:   transfer.RecipientAccountID,
		Amount:        transfer.Amount,
	})
	if err == nil {
		updated, dbErr := markTransferCompleted(ctx, pool, transfer.ID, resp.GetTransactionId())
		if dbErr != nil {
			return Transfer{}, 0, dbErr
		}
		return updated, settlementCompleted, nil
	}

	var failureReason string
	switch status.Code(err) {
	case codes.FailedPrecondition:
		failureReason = failureReasonInsufficientFunds
	case codes.NotFound:
		failureReason = failureReasonAccountNotFound
	case codes.InvalidArgument:
		failureReason = failureReasonInvalidAmount
	case codes.Internal:
		failureReason = failureReasonLedgerInternal
	default:
		// codes.Unavailable, codes.DeadlineExceeded, codes.Unknown: we do
		// not know whether the transfer executed. Leave the row exactly as
		// it is (pending) — do not write anything.
		return transfer, settlementUncertain, nil
	}

	updated, dbErr := markTransferFailed(ctx, pool, transfer.ID, failureReason)
	if dbErr != nil {
		return Transfer{}, 0, dbErr
	}
	return updated, settlementFailed, nil
}

// markTransferCompleted and markTransferFailed don't distinguish sender vs.
// recipient on a ledger NotFound (ledger-svc's message text does — "from
// account not found" vs "to account not found" — but that's a string, not a
// structured detail). Both accounts are already validated to exist by
// createTransfer, so this path should be unreachable in practice; a single
// generic account_not_found reason is enough rather than parsing
// ledger-svc's message wording, a fragile coupling.

func markTransferCompleted(ctx context.Context, pool *pgxpool.Pool, id, ledgerTransactionID string) (Transfer, error) {
	var t Transfer
	err := pool.QueryRow(ctx,
		`UPDATE transfers SET status = 'completed', ledger_transaction_id = $1, updated_at = now()
		 WHERE id = $2
		 RETURNING id, idempotency_key, sender_account_id, recipient_account_id, amount, status, failure_reason, ledger_transaction_id, created_at, updated_at`,
		ledgerTransactionID, id,
	).Scan(&t.ID, &t.IdempotencyKey, &t.SenderAccountID, &t.RecipientAccountID, &t.Amount, &t.Status, &t.FailureReason, &t.LedgerTransactionID, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return Transfer{}, fmt.Errorf("mark transfer completed: %w", err)
	}
	return t, nil
}

func markTransferFailed(ctx context.Context, pool *pgxpool.Pool, id, failureReason string) (Transfer, error) {
	var t Transfer
	err := pool.QueryRow(ctx,
		`UPDATE transfers SET status = 'failed', failure_reason = $1, updated_at = now()
		 WHERE id = $2
		 RETURNING id, idempotency_key, sender_account_id, recipient_account_id, amount, status, failure_reason, ledger_transaction_id, created_at, updated_at`,
		failureReason, id,
	).Scan(&t.ID, &t.IdempotencyKey, &t.SenderAccountID, &t.RecipientAccountID, &t.Amount, &t.Status, &t.FailureReason, &t.LedgerTransactionID, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return Transfer{}, fmt.Errorf("mark transfer failed: %w", err)
	}
	return t, nil
}

// randomUUID mints a UUID v4 by hand, matching this repo's existing
// convention (e.g. auth-svc's generateEventID, ledger_test.go's randomUUID)
// of hand-rolling UUIDs rather than adding a uuid dependency.
func randomUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx (RFC 4122)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
