package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountsv1 "neobank/proto/gen/go/accounts/v1"
	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

// genesisAccountID is ledger-svc's fixed genesis account_id
// (services/ledger-svc/ledger.go), duplicated here for the same reason
// it's already duplicated across cmd/seed and cmd/devtopup in that
// service: it's a well-known constant, not a secret, and every existing
// duplicate is intentional rather than an oversight to fix.
const genesisAccountID = "00000000-0000-0000-0000-000000000001"

const (
	// withdrawMinAmount/withdrawMaxAmount mirror depositMinAmount/
	// depositMaxAmount (deposit.go) — same reasoning, same values: a
	// floor against a call too small to be meaningful and a ceiling
	// against a fat-fingered amount.
	withdrawMinAmount int64 = 50
	withdrawMaxAmount int64 = 1_000_000 // €10,000.00
)

// Withdrawal is a withdrawals row — see createWithdrawal's doc comment
// for the SIMULATION-ONLY scope this represents. Unlike Deposit, there is
// no 'pending' status: the whole operation is synchronous (an ordinary
// ExecuteTransfer call, not an external API round trip), so a row is
// only ever written once its outcome is already known.
type Withdrawal struct {
	ID                  string    `json:"id"`
	AccountID           string    `json:"account_id"`
	Amount              int64     `json:"amount"`
	Status              string    `json:"status"`
	LedgerTransactionID *string   `json:"ledger_transaction_id,omitempty"`
	FailureReason       *string   `json:"failure_reason,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

const withdrawalColumns = `id, account_id, amount, status, ledger_transaction_id, failure_reason, created_at, updated_at`

func scanWithdrawal(row rowScanner, w *Withdrawal) error {
	return row.Scan(&w.ID, &w.AccountID, &w.Amount, &w.Status, &w.LedgerTransactionID, &w.FailureReason, &w.CreatedAt, &w.UpdatedAt)
}

type createWithdrawalOutcome int

const (
	createWithdrawalPayoutSimulated createWithdrawalOutcome = iota
	createWithdrawalInvalidAmount
	createWithdrawalAccountNotActive
	createWithdrawalInsufficientFunds
)

// createWithdrawal is a SIMULATION ONLY. A real payout — money actually
// leaving the system to a card or bank account — requires money
// transmitter licensing this project does not have and will not acquire
// for a portfolio piece; Stripe Connect/payout APIs are never called
// here, not even in test mode. What this genuinely does: debit
// accountID's internal ledger balance via the same ordinary,
// balance-checked ExecuteTransfer every transfer uses (account ->
// genesis — genesis absorbing the credit side the same way it absorbs
// every other internal movement), and record the attempt with status
// 'payout_simulated'. See README for the full reasoning and the UI-side
// disclaimer requirement.
func createWithdrawal(ctx context.Context, pool *pgxpool.Pool, accountsClient accountsv1.AccountsServiceClient, ledgerClient ledgerv1.LedgerServiceClient, accountID string, amount int64) (withdrawal Withdrawal, outcome createWithdrawalOutcome, err error) {
	if amount < withdrawMinAmount || amount > withdrawMaxAmount {
		return Withdrawal{}, createWithdrawalInvalidAmount, nil
	}

	account, err := accountsClient.GetAccountByID(ctx, &accountsv1.GetAccountByIDRequest{AccountId: accountID})
	if err != nil {
		return Withdrawal{}, 0, fmt.Errorf("look up account: %w", err)
	}
	if account.GetStatus() != accountStatusActive {
		return Withdrawal{}, createWithdrawalAccountNotActive, nil
	}

	resp, err := ledgerClient.ExecuteTransfer(ctx, &ledgerv1.ExecuteTransferRequest{
		FromAccountId: accountID,
		ToAccountId:   genesisAccountID,
		Amount:        amount,
	})
	if err != nil {
		if status.Code(err) == codes.FailedPrecondition {
			reason := "insufficient_funds"
			w, insertErr := insertWithdrawal(ctx, pool, accountID, amount, "failed", nil, &reason)
			if insertErr != nil {
				return Withdrawal{}, 0, fmt.Errorf("record failed withdrawal: %w", insertErr)
			}
			return w, createWithdrawalInsufficientFunds, nil
		}
		return Withdrawal{}, 0, fmt.Errorf("execute transfer: %w", err)
	}

	ledgerTransactionID := resp.GetTransactionId()
	w, err := insertWithdrawal(ctx, pool, accountID, amount, "payout_simulated", &ledgerTransactionID, nil)
	if err != nil {
		return Withdrawal{}, 0, fmt.Errorf("record simulated withdrawal: %w", err)
	}
	return w, createWithdrawalPayoutSimulated, nil
}

func insertWithdrawal(ctx context.Context, pool *pgxpool.Pool, accountID string, amount int64, withdrawalStatus string, ledgerTransactionID, failureReason *string) (Withdrawal, error) {
	var w Withdrawal
	err := scanWithdrawal(pool.QueryRow(ctx,
		`INSERT INTO withdrawals (account_id, amount, status, ledger_transaction_id, failure_reason)
		 VALUES ($1, $2, $3, $4, $5) RETURNING `+withdrawalColumns,
		accountID, amount, withdrawalStatus, ledgerTransactionID, failureReason,
	), &w)
	if err != nil {
		return Withdrawal{}, fmt.Errorf("insert withdrawal: %w", err)
	}
	return w, nil
}
