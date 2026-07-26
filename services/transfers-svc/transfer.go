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
)

const (
	accountStatusActive = "active"
	accountStatusClosed = "closed"
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
