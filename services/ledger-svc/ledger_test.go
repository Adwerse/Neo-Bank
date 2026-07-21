package main

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// newTestPool connects to the postgres instance from docker-compose.yml via
// DATABASE_URL, skipping the test if it isn't set — these tests exercise
// real SQL (SUM, COALESCE, casts), not something worth mocking out.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set; skipping test that requires a live postgres (see docker-compose.yml)")
	}

	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// randomUUID mints a UUID v4 by hand, matching auth-svc's generateEventID
// convention (kafka.go) — this repo hand-rolls UUIDs in Go rather than add
// github.com/google/uuid as a dependency.
func randomUUID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		t.Fatalf("generate random uuid: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx (RFC 4122)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// insertLedgerAccount creates a ledger_accounts row for accountID and
// registers cleanup of it (and any entries against it) once the test ends.
func insertLedgerAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID string) string {
	t.Helper()
	var ledgerAccountID string
	err := pool.QueryRow(ctx,
		"INSERT INTO ledger_accounts (account_id) VALUES ($1) RETURNING id",
		accountID,
	).Scan(&ledgerAccountID)
	if err != nil {
		t.Fatalf("insert ledger account: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM entries WHERE ledger_account_id = $1", ledgerAccountID); err != nil {
			t.Logf("cleanup: delete entries for ledger_account_id=%s: %v", ledgerAccountID, err)
		}
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM ledger_accounts WHERE id = $1", ledgerAccountID); err != nil {
			t.Logf("cleanup: delete ledger_account id=%s: %v", ledgerAccountID, err)
		}
	})
	return ledgerAccountID
}

func insertEntry(t *testing.T, ctx context.Context, pool *pgxpool.Pool, transactionID, ledgerAccountID string, amount int64) {
	t.Helper()
	_, err := pool.Exec(ctx,
		"INSERT INTO entries (transaction_id, ledger_account_id, amount) VALUES ($1, $2, $3)",
		transactionID, ledgerAccountID, amount,
	)
	if err != nil {
		t.Fatalf("insert entry: %v", err)
	}
}

func TestGetBalance(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	testAccountID := randomUUID(t)
	counterpartyAccountID := randomUUID(t)

	testLedgerID := insertLedgerAccount(t, ctx, pool, testAccountID)
	counterpartyLedgerID := insertLedgerAccount(t, ctx, pool, counterpartyAccountID)

	// Two transactions, each a balanced debit/credit pair against a
	// counterparty account, so the global sum-zero invariant holds even
	// while this test's fixture data exists in the shared dev database.
	insertEntry(t, ctx, pool, randomUUID(t), testLedgerID, 10000)
	insertEntry(t, ctx, pool, randomUUID(t), counterpartyLedgerID, -10000)

	insertEntry(t, ctx, pool, randomUUID(t), testLedgerID, -3000)
	insertEntry(t, ctx, pool, randomUUID(t), counterpartyLedgerID, 3000)

	balance, err := getBalance(ctx, pool, testAccountID)
	if err != nil {
		t.Fatalf("getBalance: unexpected error: %v", err)
	}
	if balance != 7000 {
		t.Errorf("getBalance = %d, want 7000", balance)
	}
}

func TestGetBalance_NotFound(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	_, err := getBalance(ctx, pool, randomUUID(t))
	if !errors.Is(err, ErrLedgerAccountNotFound) {
		t.Fatalf("getBalance for a nonexistent account_id = %v, want ErrLedgerAccountNotFound", err)
	}
}

func TestGetBalance_ZeroForAccountWithNoEntries(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUID(t)
	insertLedgerAccount(t, ctx, pool, accountID)

	balance, err := getBalance(ctx, pool, accountID)
	if err != nil {
		t.Fatalf("getBalance: unexpected error: %v", err)
	}
	if balance != 0 {
		t.Errorf("getBalance = %d, want 0 (account exists but has no entries)", balance)
	}
}

// entryCount returns how many entries rows exist for ledgerAccountID, used
// to assert that a rejected transfer wrote nothing at all (not just that
// balances are unchanged, which a bogus offsetting pair could also satisfy).
func entryCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ledgerAccountID string) int {
	t.Helper()
	var count int
	err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM entries WHERE ledger_account_id = $1", ledgerAccountID).Scan(&count)
	if err != nil {
		t.Fatalf("count entries: %v", err)
	}
	return count
}

// fundAccount gives ledgerAccountID a starting balance by inserting a
// balanced pair against a throwaway counterparty account, preserving the
// global SUM(entries) = 0 invariant in the shared dev database.
func fundAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ledgerAccountID string, amount int64) {
	t.Helper()
	counterpartyAccountID := randomUUID(t)
	counterpartyLedgerID := insertLedgerAccount(t, ctx, pool, counterpartyAccountID)
	insertEntry(t, ctx, pool, randomUUID(t), ledgerAccountID, amount)
	insertEntry(t, ctx, pool, randomUUID(t), counterpartyLedgerID, -amount)
}

func TestExecuteTransfer_Success(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	fromAccountID := randomUUID(t)
	toAccountID := randomUUID(t)
	fromLedgerID := insertLedgerAccount(t, ctx, pool, fromAccountID)
	toLedgerID := insertLedgerAccount(t, ctx, pool, toAccountID)
	fundAccount(t, ctx, pool, fromLedgerID, 10000)

	transactionID, outcome, err := executeTransfer(ctx, pool, fromAccountID, toAccountID, 3000)
	if err != nil {
		t.Fatalf("executeTransfer: unexpected error: %v", err)
	}
	if outcome != transferOK {
		t.Fatalf("executeTransfer outcome = %v, want transferOK", outcome)
	}
	if transactionID == "" {
		t.Fatal("executeTransfer: transactionID is empty on success")
	}

	fromBalance, err := getBalance(ctx, pool, fromAccountID)
	if err != nil {
		t.Fatalf("getBalance(from): unexpected error: %v", err)
	}
	if fromBalance != 7000 {
		t.Errorf("from balance = %d, want 7000", fromBalance)
	}
	toBalance, err := getBalance(ctx, pool, toAccountID)
	if err != nil {
		t.Fatalf("getBalance(to): unexpected error: %v", err)
	}
	if toBalance != 3000 {
		t.Errorf("to balance = %d, want 3000", toBalance)
	}

	rows, err := pool.Query(ctx, "SELECT ledger_account_id, amount FROM entries WHERE transaction_id = $1", transactionID)
	if err != nil {
		t.Fatalf("query entries for transaction_id=%s: %v", transactionID, err)
	}
	defer rows.Close()
	var sum int64
	var n int
	for rows.Next() {
		var ledgerAccountID string
		var amount int64
		if err := rows.Scan(&ledgerAccountID, &amount); err != nil {
			t.Fatalf("scan entry: %v", err)
		}
		switch ledgerAccountID {
		case fromLedgerID:
			if amount != -3000 {
				t.Errorf("from entry amount = %d, want -3000", amount)
			}
		case toLedgerID:
			if amount != 3000 {
				t.Errorf("to entry amount = %d, want 3000", amount)
			}
		default:
			t.Errorf("unexpected ledger_account_id %s in transaction %s", ledgerAccountID, transactionID)
		}
		sum += amount
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate entries: %v", err)
	}
	if n != 2 {
		t.Errorf("entries for transaction_id=%s: got %d rows, want 2", transactionID, n)
	}
	if sum != 0 {
		t.Errorf("SUM(entries) for transaction_id=%s = %d, want 0", transactionID, sum)
	}
}

func TestExecuteTransfer_InsufficientFunds(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	fromAccountID := randomUUID(t)
	toAccountID := randomUUID(t)
	fromLedgerID := insertLedgerAccount(t, ctx, pool, fromAccountID)
	toLedgerID := insertLedgerAccount(t, ctx, pool, toAccountID)
	fundAccount(t, ctx, pool, fromLedgerID, 1000)

	fromCountBefore := entryCount(t, ctx, pool, fromLedgerID)
	toCountBefore := entryCount(t, ctx, pool, toLedgerID)

	transactionID, outcome, err := executeTransfer(ctx, pool, fromAccountID, toAccountID, 5000)
	if err != nil {
		t.Fatalf("executeTransfer: unexpected error: %v", err)
	}
	if outcome != transferInsufficientFunds {
		t.Fatalf("executeTransfer outcome = %v, want transferInsufficientFunds", outcome)
	}
	if transactionID != "" {
		t.Errorf("executeTransfer: transactionID = %q, want empty on failure", transactionID)
	}

	fromBalance, err := getBalance(ctx, pool, fromAccountID)
	if err != nil {
		t.Fatalf("getBalance(from): unexpected error: %v", err)
	}
	if fromBalance != 1000 {
		t.Errorf("from balance = %d, want unchanged 1000", fromBalance)
	}
	toBalance, err := getBalance(ctx, pool, toAccountID)
	if err != nil {
		t.Fatalf("getBalance(to): unexpected error: %v", err)
	}
	if toBalance != 0 {
		t.Errorf("to balance = %d, want unchanged 0", toBalance)
	}

	if got := entryCount(t, ctx, pool, fromLedgerID); got != fromCountBefore {
		t.Errorf("from entry count = %d, want unchanged %d", got, fromCountBefore)
	}
	if got := entryCount(t, ctx, pool, toLedgerID); got != toCountBefore {
		t.Errorf("to entry count = %d, want unchanged %d", got, toCountBefore)
	}
}

func TestExecuteTransfer_InvalidAmount(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	fromAccountID := randomUUID(t)
	toAccountID := randomUUID(t)

	for _, amount := range []int64{0, -100} {
		transactionID, outcome, err := executeTransfer(ctx, pool, fromAccountID, toAccountID, amount)
		if err != nil {
			t.Fatalf("executeTransfer(amount=%d): unexpected error: %v", amount, err)
		}
		if outcome != transferInvalidAmount {
			t.Errorf("executeTransfer(amount=%d) outcome = %v, want transferInvalidAmount", amount, outcome)
		}
		if transactionID != "" {
			t.Errorf("executeTransfer(amount=%d): transactionID = %q, want empty", amount, transactionID)
		}
	}
}

func TestExecuteTransfer_FromAccountNotFound(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	toAccountID := randomUUID(t)
	insertLedgerAccount(t, ctx, pool, toAccountID)

	_, outcome, err := executeTransfer(ctx, pool, randomUUID(t), toAccountID, 100)
	if err != nil {
		t.Fatalf("executeTransfer: unexpected error: %v", err)
	}
	if outcome != transferFromAccountNotFound {
		t.Errorf("executeTransfer outcome = %v, want transferFromAccountNotFound", outcome)
	}
}

func TestExecuteTransfer_ToAccountNotFound(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	fromAccountID := randomUUID(t)
	fromLedgerID := insertLedgerAccount(t, ctx, pool, fromAccountID)
	fundAccount(t, ctx, pool, fromLedgerID, 1000)

	_, outcome, err := executeTransfer(ctx, pool, fromAccountID, randomUUID(t), 100)
	if err != nil {
		t.Fatalf("executeTransfer: unexpected error: %v", err)
	}
	if outcome != transferToAccountNotFound {
		t.Errorf("executeTransfer outcome = %v, want transferToAccountNotFound", outcome)
	}
}
