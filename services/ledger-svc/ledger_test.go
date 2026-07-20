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
