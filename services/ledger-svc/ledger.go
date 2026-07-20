package main

import (
	"context"
	"errors"
	"fmt"

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
