package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrLedgerAccountNotFound indicates no ledger_accounts row exists for the
// given account_id. It is distinct from a genuinely zero balance: an
// account that exists but has no entries (or entries summing to zero) is a
// valid state, not an error.
var ErrLedgerAccountNotFound = errors.New("ledger account not found")

// getBalance computes accountID's balance by summing its entries. The
// balance is never stored as a mutable field — it is always derived from
// the append-only entries log, which is the essence of event sourcing:
// state is derived from history, never cached alongside it.
//
// SUM(amount) is cast to bigint in SQL because Postgres's SUM(bigint)
// returns numeric, which pgx cannot scan directly into an int64.
func getBalance(ctx context.Context, pool *pgxpool.Pool, accountID string) (int64, error) {
	var ledgerAccountID string
	err := pool.QueryRow(ctx,
		"SELECT id FROM ledger_accounts WHERE account_id = $1",
		accountID,
	).Scan(&ledgerAccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrLedgerAccountNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("look up ledger account: %w", err)
	}

	var balance int64
	err = pool.QueryRow(ctx,
		"SELECT COALESCE(SUM(amount), 0)::bigint FROM entries WHERE ledger_account_id = $1",
		ledgerAccountID,
	).Scan(&balance)
	if err != nil {
		return 0, fmt.Errorf("sum entries: %w", err)
	}
	return balance, nil
}

// transferOutcome distinguishes the business-level results of
// executeTransfer. Only genuinely unexpected failures (a lost DB
// connection, a malformed query) are reported via the error return instead;
// every outcome below is an expected, non-exceptional branch.
type transferOutcome int

const (
	transferOK transferOutcome = iota
	transferInvalidAmount
	transferFromAccountNotFound
	transferToAccountNotFound
	transferInsufficientFunds
)

// lockLedgerAccount looks up and FOR-UPDATE-locks the ledger_accounts row
// for accountID within tx. Callers transferring funds between two accounts
// must lock both sides in ascending account_id order (see executeTransfer)
// so that two concurrent transfers running in opposite directions between
// the same pair of accounts always attempt to lock the same row first,
// rather than deadlocking on each other.
func lockLedgerAccount(ctx context.Context, tx pgx.Tx, accountID string) (ledgerAccountID string, found bool, err error) {
	err = tx.QueryRow(ctx,
		"SELECT id FROM ledger_accounts WHERE account_id = $1 FOR UPDATE",
		accountID,
	).Scan(&ledgerAccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("lock ledger account: %w", err)
	}
	return ledgerAccountID, true, nil
}

// executeTransfer atomically moves amount from fromAccountID to
// toAccountID by inserting a balanced debit/credit pair of entries sharing
// one transaction_id. The global SUM(entries) = 0 invariant holds
// automatically because the pair always nets to zero; the "no account goes
// negative" invariant is enforced by checking fromAccountID's balance
// inside the same transaction that inserts the entries.
//
// Concurrency: both accounts are locked FOR UPDATE, in ascending
// account_id order, before the balance check. Locking prevents the race
// where two concurrent transfers both read the same stale balance and
// jointly overdraw the account — the second transfer's lock acquisition
// blocks until the first commits or rolls back, so it always evaluates the
// balance check against up-to-date state. Locking in a fixed order
// (independent of transfer direction) prevents deadlock between two
// concurrent transfers going in opposite directions between the same pair
// of accounts, since both will always attempt to lock the same account
// first rather than each holding one lock and waiting on the other.
func executeTransfer(ctx context.Context, pool *pgxpool.Pool, fromAccountID, toAccountID string, amount int64) (transactionID string, outcome transferOutcome, err error) {
	if amount <= 0 {
		return "", transferInvalidAmount, nil
	}

	first, second := fromAccountID, toAccountID
	if strings.ToLower(toAccountID) < strings.ToLower(fromAccountID) {
		first, second = toAccountID, fromAccountID
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", 0, err
	}
	defer tx.Rollback(ctx)

	locked := make(map[string]string, 2) // account_id -> ledger_accounts.id
	for _, accountID := range [2]string{first, second} {
		ledgerAccountID, found, lockErr := lockLedgerAccount(ctx, tx, accountID)
		if lockErr != nil {
			return "", 0, lockErr
		}
		if !found {
			if accountID == fromAccountID {
				return "", transferFromAccountNotFound, nil
			}
			return "", transferToAccountNotFound, nil
		}
		locked[accountID] = ledgerAccountID
	}
	fromLedgerAccountID := locked[fromAccountID]
	toLedgerAccountID := locked[toAccountID]

	var fromBalance int64
	err = tx.QueryRow(ctx,
		"SELECT COALESCE(SUM(amount), 0)::bigint FROM entries WHERE ledger_account_id = $1",
		fromLedgerAccountID,
	).Scan(&fromBalance)
	if err != nil {
		return "", 0, fmt.Errorf("sum entries: %w", err)
	}
	if fromBalance < amount {
		return "", transferInsufficientFunds, nil
	}

	err = tx.QueryRow(ctx,
		`INSERT INTO entries (transaction_id, ledger_account_id, amount)
		 VALUES (gen_random_uuid(), $1, $2)
		 RETURNING transaction_id`,
		fromLedgerAccountID, -amount,
	).Scan(&transactionID)
	if err != nil {
		return "", 0, fmt.Errorf("insert debit entry: %w", err)
	}

	_, err = tx.Exec(ctx,
		"INSERT INTO entries (transaction_id, ledger_account_id, amount) VALUES ($1, $2, $3)",
		transactionID, toLedgerAccountID, amount,
	)
	if err != nil {
		return "", 0, fmt.Errorf("insert credit entry: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", 0, fmt.Errorf("commit transfer: %w", err)
	}
	return transactionID, transferOK, nil
}
