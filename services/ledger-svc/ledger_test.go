package main

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

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
// registers cleanup of it (and any entries or cached balance against it)
// once the test ends.
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
		if _, err := pool.Exec(cleanupCtx, "DELETE FROM account_balances WHERE ledger_account_id = $1", ledgerAccountID); err != nil {
			t.Logf("cleanup: delete account_balances for ledger_account_id=%s: %v", ledgerAccountID, err)
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

// insertEntryAt is insertEntry with an explicit created_at, for tests that
// need entries spread across specific days — getBalanceHistory's
// day-bucketing can't otherwise be exercised, since insertEntry (used by
// ~20 other tests, left untouched here) always lands on the DB's now().
func insertEntryAt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, transactionID, ledgerAccountID string, amount int64, createdAt time.Time) {
	t.Helper()
	_, err := pool.Exec(ctx,
		"INSERT INTO entries (transaction_id, ledger_account_id, amount, created_at) VALUES ($1, $2, $3, $4)",
		transactionID, ledgerAccountID, amount, createdAt,
	)
	if err != nil {
		t.Fatalf("insert entry at %v: %v", createdAt, err)
	}
}

// TestCreateLedgerAccount_CreatesThenIdempotent proves CreateLedgerAccount's
// idempotency contract: the first call creates the ledger_accounts row, and a
// second call for the same account_id returns the same row rather than
// erroring on the account_id UNIQUE constraint — the property accounts-svc
// relies on when a redelivered UserActivated event re-runs the call.
func TestCreateLedgerAccount_CreatesThenIdempotent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUID(t)
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM ledger_accounts WHERE account_id = $1", accountID); err != nil {
			t.Logf("cleanup: delete ledger_account account_id=%s: %v", accountID, err)
		}
	})

	first, err := createLedgerAccount(ctx, pool, accountID)
	if err != nil {
		t.Fatalf("createLedgerAccount (first call): unexpected error: %v", err)
	}
	if first.AccountID != accountID {
		t.Errorf("first.AccountID = %q, want %q", first.AccountID, accountID)
	}

	second, err := createLedgerAccount(ctx, pool, accountID)
	if err != nil {
		t.Fatalf("createLedgerAccount (second call): unexpected error: %v", err)
	}
	if second.AccountID != first.AccountID {
		t.Errorf("second.AccountID = %q, want %q (same account)", second.AccountID, first.AccountID)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("second.CreatedAt = %v, want %v (existing row returned, not recreated)", second.CreatedAt, first.CreatedAt)
	}

	// Exactly one row exists — the second call did not insert a duplicate.
	var count int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM ledger_accounts WHERE account_id = $1", accountID).Scan(&count); err != nil {
		t.Fatalf("count ledger_accounts: %v", err)
	}
	if count != 1 {
		t.Errorf("ledger_accounts rows for account_id=%s = %d, want 1", accountID, count)
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

	// insertEntry writes only the log; seed the cache to match, mirroring
	// what a real balance-affecting write (executeTransfer) would have left
	// behind, so getBalance is reading account_balances in a consistent
	// state rather than exercising the "no cache row yet" fallback.
	setAccountBalance(t, ctx, pool, testLedgerID, 7000)

	balance, err := getBalance(ctx, pool, testAccountID)
	if err != nil {
		t.Fatalf("getBalance: unexpected error: %v", err)
	}
	if balance != 7000 {
		t.Errorf("getBalance = %d, want 7000", balance)
	}

	var naiveSum int64
	err = pool.QueryRow(ctx,
		"SELECT COALESCE(SUM(amount), 0)::bigint FROM entries WHERE ledger_account_id = $1",
		testLedgerID,
	).Scan(&naiveSum)
	if err != nil {
		t.Fatalf("naive sum query: %v", err)
	}
	if balance != naiveSum {
		t.Errorf("getBalance = %d, naive SUM(entries) = %d, want equal", balance, naiveSum)
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

// setAccountBalance directly overwrites ledgerAccountID's cached balance in
// account_balances, bypassing any recomputation. A raw fixture helper for
// establishing a starting cache state, either consistent with entries
// already written (mirroring what a real write already leaves behind) or
// deliberately wrong (to test that rebuildBalance fixes drift).
func setAccountBalance(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ledgerAccountID string, balance int64) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO account_balances (ledger_account_id, balance, updated_at)
		 VALUES ($1, $2, now())
		 ON CONFLICT (ledger_account_id) DO UPDATE SET balance = EXCLUDED.balance, updated_at = EXCLUDED.updated_at`,
		ledgerAccountID, balance,
	)
	if err != nil {
		t.Fatalf("set account_balances: %v", err)
	}
}

// fundAccount gives ledgerAccountID a starting balance by inserting a
// balanced pair against a throwaway counterparty account, preserving the
// global SUM(entries) = 0 invariant in the shared dev database, and seeds
// account_balances for both sides to match — insertEntry only writes the
// log, so without this the cache would start empty (not merely zero) and
// executeTransfer's incremental delta update would land on the wrong
// baseline. Returns the counterparty's ledger_accounts id, for tests that
// need to include it in their own scoped sum-zero checks.
func fundAccount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ledgerAccountID string, amount int64) string {
	t.Helper()
	counterpartyAccountID := randomUUID(t)
	counterpartyLedgerID := insertLedgerAccount(t, ctx, pool, counterpartyAccountID)
	insertEntry(t, ctx, pool, randomUUID(t), ledgerAccountID, amount)
	insertEntry(t, ctx, pool, randomUUID(t), counterpartyLedgerID, -amount)
	setAccountBalance(t, ctx, pool, ledgerAccountID, amount)
	setAccountBalance(t, ctx, pool, counterpartyLedgerID, -amount)
	return counterpartyLedgerID
}

func TestExecuteTransfer_Success(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	fromAccountID := randomUUID(t)
	toAccountID := randomUUID(t)
	fromLedgerID := insertLedgerAccount(t, ctx, pool, fromAccountID)
	toLedgerID := insertLedgerAccount(t, ctx, pool, toAccountID)
	fundAccount(t, ctx, pool, fromLedgerID, 10000)

	transactionID, outcome, err := executeTransfer(ctx, pool, fromAccountID, toAccountID, 3000, "")
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

// TestExecuteTransfer_WithReference proves a non-empty reference is stored
// on both entries a transfer writes, and that getTransactionByReference can
// then find the same transaction_id from it — the mechanism
// transfers-svc's reconciliation worker depends on to ask "did this
// transfer actually execute," independent of whether its own response was
// ever received.
func TestExecuteTransfer_WithReference(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	fromAccountID := randomUUID(t)
	toAccountID := randomUUID(t)
	fromLedgerID := insertLedgerAccount(t, ctx, pool, fromAccountID)
	insertLedgerAccount(t, ctx, pool, toAccountID)
	fundAccount(t, ctx, pool, fromLedgerID, 10000)

	reference := randomUUID(t)
	transactionID, outcome, err := executeTransfer(ctx, pool, fromAccountID, toAccountID, 1500, reference)
	if err != nil {
		t.Fatalf("executeTransfer: unexpected error: %v", err)
	}
	if outcome != transferOK {
		t.Fatalf("executeTransfer outcome = %v, want transferOK", outcome)
	}

	var referenceCount int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM entries WHERE transaction_id = $1 AND reference = $2", transactionID, reference).Scan(&referenceCount); err != nil {
		t.Fatalf("count entries with reference: %v", err)
	}
	if referenceCount != 2 {
		t.Errorf("entries with reference=%s for transaction_id=%s = %d, want 2 (both debit and credit)", reference, transactionID, referenceCount)
	}

	foundTransactionID, found, err := getTransactionByReference(ctx, pool, reference)
	if err != nil {
		t.Fatalf("getTransactionByReference: unexpected error: %v", err)
	}
	if !found {
		t.Fatal("getTransactionByReference: found = false, want true")
	}
	if foundTransactionID != transactionID {
		t.Errorf("getTransactionByReference: transactionID = %q, want %q", foundTransactionID, transactionID)
	}
}

func TestGetTransactionByReference_NotFound(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	_, found, err := getTransactionByReference(ctx, pool, randomUUID(t))
	if err != nil {
		t.Fatalf("getTransactionByReference: unexpected error: %v", err)
	}
	if found {
		t.Error("getTransactionByReference: found = true for a reference that was never used, want false")
	}
}

// TestExecuteTransfer_EmptyReferenceLeavesEntriesUnreferenced proves an
// empty reference (the default for callers like devtopup/cmd/seed that
// don't pass one) stores NULL, not the empty string — Postgres would
// reject "" as an invalid uuid, and a stored empty string would also be
// wrong: it would make every no-reference transfer collide on the same
// lookup key.
func TestExecuteTransfer_EmptyReferenceLeavesEntriesUnreferenced(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	fromAccountID := randomUUID(t)
	toAccountID := randomUUID(t)
	fromLedgerID := insertLedgerAccount(t, ctx, pool, fromAccountID)
	insertLedgerAccount(t, ctx, pool, toAccountID)
	fundAccount(t, ctx, pool, fromLedgerID, 10000)

	transactionID, outcome, err := executeTransfer(ctx, pool, fromAccountID, toAccountID, 1000, "")
	if err != nil {
		t.Fatalf("executeTransfer: unexpected error: %v", err)
	}
	if outcome != transferOK {
		t.Fatalf("executeTransfer outcome = %v, want transferOK", outcome)
	}

	var nullCount int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM entries WHERE transaction_id = $1 AND reference IS NULL", transactionID).Scan(&nullCount); err != nil {
		t.Fatalf("count entries with NULL reference: %v", err)
	}
	if nullCount != 2 {
		t.Errorf("entries with NULL reference for transaction_id=%s = %d, want 2", transactionID, nullCount)
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

	transactionID, outcome, err := executeTransfer(ctx, pool, fromAccountID, toAccountID, 5000, "")
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
		transactionID, outcome, err := executeTransfer(ctx, pool, fromAccountID, toAccountID, amount, "")
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

	_, outcome, err := executeTransfer(ctx, pool, randomUUID(t), toAccountID, 100, "")
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

	_, outcome, err := executeTransfer(ctx, pool, fromAccountID, randomUUID(t), 100, "")
	if err != nil {
		t.Fatalf("executeTransfer: unexpected error: %v", err)
	}
	if outcome != transferToAccountNotFound {
		t.Errorf("executeTransfer outcome = %v, want transferToAccountNotFound", outcome)
	}
}

// TestRebuildBalance_MatchesIncrementalCache covers the DoD requirement
// that after a series of transfers, rebuildBalance's from-scratch
// recomputation agrees with the value account_balances accumulated
// incrementally via executeTransfer's deltas — proof the cache and the log
// it's derived from have stayed in sync.
func TestRebuildBalance_MatchesIncrementalCache(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUID(t)
	ledgerID := insertLedgerAccount(t, ctx, pool, accountID)
	fundAccount(t, ctx, pool, ledgerID, 10000)

	counterpartyID := randomUUID(t)
	insertLedgerAccount(t, ctx, pool, counterpartyID)

	for _, amount := range []int64{4000, 1000, 500} {
		_, outcome, err := executeTransfer(ctx, pool, accountID, counterpartyID, amount, "")
		if err != nil {
			t.Fatalf("executeTransfer(%d): unexpected error: %v", amount, err)
		}
		if outcome != transferOK {
			t.Fatalf("executeTransfer(%d) outcome = %v, want transferOK", amount, outcome)
		}
	}

	const wantBalance = 10000 - 4000 - 1000 - 500
	incremental, err := getBalance(ctx, pool, accountID)
	if err != nil {
		t.Fatalf("getBalance: unexpected error: %v", err)
	}
	if incremental != wantBalance {
		t.Fatalf("incrementally-cached balance = %d, want %d", incremental, wantBalance)
	}

	rebuilt, err := rebuildBalance(ctx, pool, accountID)
	if err != nil {
		t.Fatalf("rebuildBalance: unexpected error: %v", err)
	}
	if rebuilt != incremental {
		t.Errorf("rebuildBalance = %d, want %d (same as incrementally-accumulated cache)", rebuilt, incremental)
	}

	afterRebuild, err := getBalance(ctx, pool, accountID)
	if err != nil {
		t.Fatalf("getBalance after rebuild: unexpected error: %v", err)
	}
	if afterRebuild != incremental {
		t.Errorf("getBalance after rebuild = %d, want %d", afterRebuild, incremental)
	}
}

// TestRebuildBalance_FixesDrift proves the cache is genuinely derived from
// (and repairable from) the log: entries are written directly, bypassing
// account_balances entirely, and the cache is then set to a deliberately
// wrong value. rebuildBalance must ignore that wrong value and recompute
// from entries — the log wins, per the stated design principle.
func TestRebuildBalance_FixesDrift(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUID(t)
	ledgerID := insertLedgerAccount(t, ctx, pool, accountID)

	insertEntry(t, ctx, pool, randomUUID(t), ledgerID, 5000)
	insertEntry(t, ctx, pool, randomUUID(t), ledgerID, -1200)
	setAccountBalance(t, ctx, pool, ledgerID, 999999) // deliberately wrong

	if got, err := getBalance(ctx, pool, accountID); err != nil || got != 999999 {
		t.Fatalf("precondition: getBalance = %d, err=%v, want drifted 999999", got, err)
	}

	const wantBalance = 5000 - 1200
	rebuilt, err := rebuildBalance(ctx, pool, accountID)
	if err != nil {
		t.Fatalf("rebuildBalance: unexpected error: %v", err)
	}
	if rebuilt != wantBalance {
		t.Errorf("rebuildBalance = %d, want %d (recomputed from entries log)", rebuilt, wantBalance)
	}

	fixed, err := getBalance(ctx, pool, accountID)
	if err != nil {
		t.Fatalf("getBalance after rebuild: unexpected error: %v", err)
	}
	if fixed != wantBalance {
		t.Errorf("getBalance after rebuild = %d, want %d", fixed, wantBalance)
	}
}

func TestRebuildBalance_NotFound(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	_, err := rebuildBalance(ctx, pool, randomUUID(t))
	if !errors.Is(err, ErrLedgerAccountNotFound) {
		t.Fatalf("rebuildBalance for a nonexistent account_id = %v, want ErrLedgerAccountNotFound", err)
	}
}

// TestExecuteTransfer_ConcurrentOverdraftPrevention is the proof that
// executeTransfer's FOR UPDATE locking (see the concurrency doc comment on
// executeTransfer in ledger.go) actually serializes concurrent debits
// against the same account, rather than merely claiming to. It fires
// goroutines concurrently instead of exercising the locking logic
// sequentially — a sequential call sequence would pass even with the
// locking code deleted entirely, since removing FOR UPDATE only produces
// wrong results under genuine concurrent access to the same account.
//
// 20 goroutines each try to withdraw 1000 from an account funded with
// exactly 10000 — enough for exactly 10 of them. Without the lock, two (or
// more) goroutines could both compute SUM(entries) before either commits
// its debit, both see the same pre-debit balance, both conclude there's
// enough, and both proceed: the account overdraws. With the lock, every
// goroutine but one blocks on its SELECT ... FOR UPDATE for the account's
// ledger_accounts row until the transaction currently holding it commits
// or rolls back — so every balance check is always against fully
// up-to-date, already-committed state. Exactly 10 succeed, exactly 10 see
// insufficient funds, and the account can never go negative.
func TestExecuteTransfer_ConcurrentOverdraftPrevention(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	const (
		startingBalance = 10000
		amount          = 1000
		attempts        = 20
		wantSucceeded   = startingBalance / amount
	)

	fromAccountID := randomUUID(t)
	fromLedgerID := insertLedgerAccount(t, ctx, pool, fromAccountID)
	counterpartyLedgerID := fundAccount(t, ctx, pool, fromLedgerID, startingBalance)

	toAccountIDs := make([]string, attempts)
	toLedgerIDs := make([]string, attempts)
	for i := range toAccountIDs {
		toAccountIDs[i] = randomUUID(t)
		toLedgerIDs[i] = insertLedgerAccount(t, ctx, pool, toAccountIDs[i])
	}

	// Each goroutine writes only to its own index — no shared mutable
	// state besides these pre-sized slices, so no extra synchronization
	// is needed to safely collect results.
	outcomes := make([]transferOutcome, attempts)
	errs := make([]error, attempts)

	var wg sync.WaitGroup
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func(i int) {
			defer wg.Done()
			_, outcome, err := executeTransfer(ctx, pool, fromAccountID, toAccountIDs[i], amount, "")
			outcomes[i] = outcome
			errs[i] = err
		}(i)
	}
	wg.Wait()

	var succeeded, insufficientFunds int
	for i, outcome := range outcomes {
		if errs[i] != nil {
			// FOR UPDATE blocks rather than aborting, so this should never
			// happen — unlike a SERIALIZABLE-based approach, there's no
			// 40001 serialization_failure to retry here. A non-nil error
			// means the locking isn't behaving as designed.
			t.Fatalf("goroutine %d: unexpected error: %v", i, errs[i])
		}
		switch outcome {
		case transferOK:
			succeeded++
		case transferInsufficientFunds:
			insufficientFunds++
		default:
			t.Fatalf("goroutine %d: unexpected outcome %v", i, outcome)
		}
	}

	if succeeded != wantSucceeded {
		t.Errorf("succeeded = %d, want %d", succeeded, wantSucceeded)
	}
	if insufficientFunds != attempts-wantSucceeded {
		t.Errorf("insufficientFunds = %d, want %d", insufficientFunds, attempts-wantSucceeded)
	}

	finalBalance, err := getBalance(ctx, pool, fromAccountID)
	if err != nil {
		t.Fatalf("getBalance: unexpected error: %v", err)
	}
	if finalBalance < 0 {
		t.Fatalf("account went negative: balance = %d — the race prevention failed", finalBalance)
	}
	if finalBalance != 0 {
		t.Errorf("final balance = %d, want exactly 0", finalBalance)
	}

	// Global SUM(entries) = 0, scoped to exactly the ledger accounts this
	// test touched: the shared dev database holds other tests' and other
	// services' balanced entries too, so summing the whole table would be
	// correct in principle but fragile to unrelated concurrent state.
	// Every entry among these accounts came from a balanced pair entirely
	// contained within this set (the initial funding, and each transfer
	// attempt), so the scoped sum is exactly as meaningful a check of the
	// invariant as a true global one.
	scopedLedgerIDs := append([]string{fromLedgerID, counterpartyLedgerID}, toLedgerIDs...)
	var totalSum int64
	err = pool.QueryRow(ctx,
		"SELECT COALESCE(SUM(amount), 0)::bigint FROM entries WHERE ledger_account_id = ANY($1::uuid[])",
		scopedLedgerIDs,
	).Scan(&totalSum)
	if err != nil {
		t.Fatalf("sum entries across storm accounts: %v", err)
	}
	if totalSum != 0 {
		t.Errorf("SUM(entries) across all accounts touched by this test = %d, want 0", totalSum)
	}
}

// cleanupDepositEntries deletes the entries deposit wrote for transactionID
// and rebuilds genesis's cached account_balances afterward. Unlike every
// other account in this file, genesis is a fixed, shared row (provisioned
// by migration 000005, not created fresh per test via insertLedgerAccount)
// that tests can't simply tear down wholesale — other tests and real
// dev-tool usage (cmd/seed, cmd/devtopup) depend on it continuing to exist.
// Deleting just this test's entries and calling the already-proven
// rebuildBalance keeps genesis's cache consistent with its log without
// hand-rolling delta arithmetic.
func cleanupDepositEntries(t *testing.T, pool *pgxpool.Pool, transactionID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DELETE FROM entries WHERE transaction_id = $1", transactionID); err != nil {
		t.Logf("cleanup: delete deposit entries for transaction_id=%s: %v", transactionID, err)
	}
	if _, err := rebuildBalance(ctx, pool, genesisAccountID); err != nil {
		t.Logf("cleanup: rebuild genesis cached balance: %v", err)
	}
}

// TestDeposit_Success proves deposit credits toAccountID by amount and
// debits genesis by the same amount in one balanced pair of entries sharing
// a transaction_id. Genesis's balance can already be arbitrarily negative
// from prior test runs or dev-tool usage, so this asserts the delta, not an
// absolute value.
func TestDeposit_Success(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	toAccountID := randomUUID(t)
	insertLedgerAccount(t, ctx, pool, toAccountID)

	genesisBalanceBefore, err := getBalance(ctx, pool, genesisAccountID)
	if err != nil {
		t.Fatalf("getBalance(genesis) before deposit: unexpected error (has migration 000005 run?): %v", err)
	}

	const amount = 4200
	transactionID, outcome, err := deposit(ctx, pool, toAccountID, amount, "")
	if err != nil {
		t.Fatalf("deposit: unexpected error: %v", err)
	}
	if outcome != depositOK {
		t.Fatalf("deposit outcome = %v, want depositOK", outcome)
	}
	if transactionID == "" {
		t.Fatal("deposit: transactionID is empty on success")
	}
	t.Cleanup(func() { cleanupDepositEntries(t, pool, transactionID) })

	toBalance, err := getBalance(ctx, pool, toAccountID)
	if err != nil {
		t.Fatalf("getBalance(to): unexpected error: %v", err)
	}
	if toBalance != amount {
		t.Errorf("to balance = %d, want %d", toBalance, amount)
	}

	genesisBalanceAfter, err := getBalance(ctx, pool, genesisAccountID)
	if err != nil {
		t.Fatalf("getBalance(genesis) after deposit: unexpected error: %v", err)
	}
	if genesisBalanceAfter != genesisBalanceBefore-amount {
		t.Errorf("genesis balance = %d, want %d (before %d minus deposited %d)", genesisBalanceAfter, genesisBalanceBefore-amount, genesisBalanceBefore, amount)
	}

	rows, err := pool.Query(ctx, "SELECT amount FROM entries WHERE transaction_id = $1", transactionID)
	if err != nil {
		t.Fatalf("query entries for transaction_id=%s: %v", transactionID, err)
	}
	defer rows.Close()
	var sum int64
	var n int
	for rows.Next() {
		var amt int64
		if err := rows.Scan(&amt); err != nil {
			t.Fatalf("scan entry: %v", err)
		}
		sum += amt
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

// TestDeposit_InvalidAmount proves deposit rejects non-positive amounts the
// same way executeTransfer does, without touching the database.
func TestDeposit_InvalidAmount(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	toAccountID := randomUUID(t)

	for _, amount := range []int64{0, -100} {
		transactionID, outcome, err := deposit(ctx, pool, toAccountID, amount, "")
		if err != nil {
			t.Fatalf("deposit(amount=%d): unexpected error: %v", amount, err)
		}
		if outcome != depositInvalidAmount {
			t.Errorf("deposit(amount=%d) outcome = %v, want depositInvalidAmount", amount, outcome)
		}
		if transactionID != "" {
			t.Errorf("deposit(amount=%d): transactionID = %q, want empty", amount, transactionID)
		}
	}
}

// TestDeposit_AccountNotFound proves a nonexistent target account is a
// reachable, expected outcome (not an error), mirroring
// executeTransfer's transferToAccountNotFound.
func TestDeposit_AccountNotFound(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	_, outcome, err := deposit(ctx, pool, randomUUID(t), 100, "")
	if err != nil {
		t.Fatalf("deposit: unexpected error: %v", err)
	}
	if outcome != depositAccountNotFound {
		t.Errorf("deposit outcome = %v, want depositAccountNotFound", outcome)
	}
}

// TestDeposit_WithReference proves reference is stored on both entries a
// deposit writes and that getTransactionByReference — the same lookup
// executeTransfer's reconciliation already relies on — finds it. This is
// the exact mechanism a future Stripe-webhook handler will use to ask "did
// I already credit this deposit.id" before crediting it again.
func TestDeposit_WithReference(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	toAccountID := randomUUID(t)
	insertLedgerAccount(t, ctx, pool, toAccountID)

	reference := randomUUID(t)
	transactionID, outcome, err := deposit(ctx, pool, toAccountID, 1500, reference)
	if err != nil {
		t.Fatalf("deposit: unexpected error: %v", err)
	}
	if outcome != depositOK {
		t.Fatalf("deposit outcome = %v, want depositOK", outcome)
	}
	t.Cleanup(func() { cleanupDepositEntries(t, pool, transactionID) })

	foundTransactionID, found, err := getTransactionByReference(ctx, pool, reference)
	if err != nil {
		t.Fatalf("getTransactionByReference: unexpected error: %v", err)
	}
	if !found {
		t.Fatal("getTransactionByReference: found = false, want true")
	}
	if foundTransactionID != transactionID {
		t.Errorf("getTransactionByReference: transactionID = %q, want %q", foundTransactionID, transactionID)
	}
}

// TestDeposit_IsIdempotentByReference proves deposit's idempotency
// contract: two calls with the same reference return the SAME
// transaction_id rather than posting twice. This matters because
// transfers-svc's crediting worker can genuinely call deposit more than
// once for the same logical deposit (e.g. it credited successfully but
// crashed before recording that locally, so the next reconciliation tick
// tries again) with no idempotency-key layer of its own above this call
// to catch it — unlike a transfer, which does have one (transfers-svc's
// own idempotency_key column).
func TestDeposit_IsIdempotentByReference(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	toAccountID := randomUUID(t)
	insertLedgerAccount(t, ctx, pool, toAccountID)
	reference := randomUUID(t)

	firstTxnID, outcome, err := deposit(ctx, pool, toAccountID, 500, reference)
	if err != nil || outcome != depositOK {
		t.Fatalf("first deposit: outcome=%v err=%v, want depositOK/nil", outcome, err)
	}
	t.Cleanup(func() { cleanupDepositEntries(t, pool, firstTxnID) })

	secondTxnID, outcome, err := deposit(ctx, pool, toAccountID, 500, reference)
	if err != nil || outcome != depositOK {
		t.Fatalf("second deposit: outcome=%v err=%v, want depositOK/nil", outcome, err)
	}

	if firstTxnID != secondTxnID {
		t.Fatalf("two deposit calls with the same reference produced different transaction_ids (%s, %s) — want the same one returned both times", firstTxnID, secondTxnID)
	}

	toBalance, err := getBalance(ctx, pool, toAccountID)
	if err != nil {
		t.Fatalf("getBalance(to): unexpected error: %v", err)
	}
	if toBalance != 500 {
		t.Errorf("to balance = %d, want 500 (the second call must not have posted a second credit)", toBalance)
	}
}

// TestDeposit_ConcurrentSameReferenceDoesNotDoublePost fires many
// concurrent deposit calls sharing one reference and proves exactly one
// entry pair gets posted — the property the transaction-scoped advisory
// lock in postUncheckedTransfer exists to guarantee, exercised under
// real concurrency rather than only sequentially (see
// TestExecuteTransfer_ConcurrentOverdraftPrevention for the same
// reasoning applied to executeTransfer's own locking).
func TestDeposit_ConcurrentSameReferenceDoesNotDoublePost(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	toAccountID := randomUUID(t)
	insertLedgerAccount(t, ctx, pool, toAccountID)
	reference := randomUUID(t)

	const attempts = 20
	transactionIDs := make([]string, attempts)
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			txnID, outcome, err := deposit(ctx, pool, toAccountID, 500, reference)
			if err != nil {
				errs[i] = err
				return
			}
			if outcome != depositOK {
				errs[i] = fmt.Errorf("outcome = %v, want depositOK", outcome)
				return
			}
			transactionIDs[i] = txnID
		}(i)
	}
	wg.Wait()

	var firstTxnID string
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
		if firstTxnID == "" {
			firstTxnID = transactionIDs[i]
		} else if transactionIDs[i] != firstTxnID {
			t.Fatalf("goroutine %d got transaction_id %s, want %s (every concurrent call must resolve to the same posting)", i, transactionIDs[i], firstTxnID)
		}
	}
	t.Cleanup(func() { cleanupDepositEntries(t, pool, firstTxnID) })

	toBalance, err := getBalance(ctx, pool, toAccountID)
	if err != nil {
		t.Fatalf("getBalance(to): unexpected error: %v", err)
	}
	if toBalance != 500 {
		t.Errorf("to balance = %d, want 500 (exactly one of %d concurrent same-reference calls must have actually posted)", toBalance, attempts)
	}
}

// TestReverseDeposit_Success proves reverseDeposit debits the target
// account and credits genesis by the same amount — the mirror image of
// deposit, checked the same way TestDeposit_Success checks deposit.
func TestReverseDeposit_Success(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUID(t)
	insertLedgerAccount(t, ctx, pool, accountID)
	depositTxnID, outcome, err := deposit(ctx, pool, accountID, 5000, "")
	if err != nil || outcome != depositOK {
		t.Fatalf("precondition deposit: outcome=%v err=%v, want depositOK/nil", outcome, err)
	}
	t.Cleanup(func() { cleanupDepositEntries(t, pool, depositTxnID) })

	genesisBalanceBefore, err := getBalance(ctx, pool, genesisAccountID)
	if err != nil {
		t.Fatalf("getBalance(genesis) before reverseDeposit: unexpected error: %v", err)
	}

	reverseTxnID, outcome, err := reverseDeposit(ctx, pool, accountID, 5000, "")
	if err != nil {
		t.Fatalf("reverseDeposit: unexpected error: %v", err)
	}
	if outcome != depositOK {
		t.Fatalf("reverseDeposit outcome = %v, want depositOK", outcome)
	}
	t.Cleanup(func() { cleanupDepositEntries(t, pool, reverseTxnID) })

	accountBalance, err := getBalance(ctx, pool, accountID)
	if err != nil {
		t.Fatalf("getBalance(account): unexpected error: %v", err)
	}
	if accountBalance != 0 {
		t.Errorf("account balance = %d, want 0 (deposited 5000, reversed 5000)", accountBalance)
	}

	genesisBalanceAfter, err := getBalance(ctx, pool, genesisAccountID)
	if err != nil {
		t.Fatalf("getBalance(genesis) after reverseDeposit: unexpected error: %v", err)
	}
	if genesisBalanceAfter != genesisBalanceBefore+5000 {
		t.Errorf("genesis balance = %d, want %d (before %d plus reversed 5000)", genesisBalanceAfter, genesisBalanceBefore+5000, genesisBalanceBefore)
	}
}

// TestReverseDeposit_AllowsNegativeBalance proves reverseDeposit does NOT
// enforce a balance check on the account being reversed — the deliberate
// behavior difference from a normal withdrawal (executeTransfer), since a
// Stripe refund already happened regardless of what the account currently
// holds in-ledger.
func TestReverseDeposit_AllowsNegativeBalance(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUID(t)
	insertLedgerAccount(t, ctx, pool, accountID)
	// No prior deposit: accountID starts at balance 0.

	transactionID, outcome, err := reverseDeposit(ctx, pool, accountID, 1500, "")
	if err != nil {
		t.Fatalf("reverseDeposit: unexpected error: %v", err)
	}
	if outcome != depositOK {
		t.Fatalf("reverseDeposit outcome = %v, want depositOK (no insufficient-funds outcome exists for this operation)", outcome)
	}
	t.Cleanup(func() { cleanupDepositEntries(t, pool, transactionID) })

	accountBalance, err := getBalance(ctx, pool, accountID)
	if err != nil {
		t.Fatalf("getBalance: unexpected error: %v", err)
	}
	if accountBalance != -1500 {
		t.Errorf("account balance = %d, want -1500", accountBalance)
	}
}

// TestReverseDeposit_IsIdempotentByReference mirrors
// TestDeposit_IsIdempotentByReference for the reversal direction: a
// redelivered charge.refunded webhook must not reverse the same deposit
// twice.
func TestReverseDeposit_IsIdempotentByReference(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUID(t)
	insertLedgerAccount(t, ctx, pool, accountID)
	reference := randomUUID(t)

	firstTxnID, outcome, err := reverseDeposit(ctx, pool, accountID, 800, reference)
	if err != nil || outcome != depositOK {
		t.Fatalf("first reverseDeposit: outcome=%v err=%v, want depositOK/nil", outcome, err)
	}
	t.Cleanup(func() { cleanupDepositEntries(t, pool, firstTxnID) })

	secondTxnID, outcome, err := reverseDeposit(ctx, pool, accountID, 800, reference)
	if err != nil || outcome != depositOK {
		t.Fatalf("second reverseDeposit: outcome=%v err=%v, want depositOK/nil", outcome, err)
	}

	if firstTxnID != secondTxnID {
		t.Fatalf("two reverseDeposit calls with the same reference produced different transaction_ids (%s, %s)", firstTxnID, secondTxnID)
	}

	accountBalance, err := getBalance(ctx, pool, accountID)
	if err != nil {
		t.Fatalf("getBalance: unexpected error: %v", err)
	}
	if accountBalance != -800 {
		t.Errorf("account balance = %d, want -800 (the second call must not have reversed a second time)", accountBalance)
	}
}

func TestGetBalanceHistory_AccountNotFound(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	_, err := getBalanceHistory(ctx, pool, randomUUID(t), nil)
	if !errors.Is(err, ErrLedgerAccountNotFound) {
		t.Fatalf("getBalanceHistory for a nonexistent account_id = %v, want ErrLedgerAccountNotFound", err)
	}
}

func TestGetBalanceHistory_EmptyAccount(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUID(t)
	insertLedgerAccount(t, ctx, pool, accountID)

	points, err := getBalanceHistory(ctx, pool, accountID, nil)
	if err != nil {
		t.Fatalf("getBalanceHistory: unexpected error: %v", err)
	}
	if len(points) != 0 {
		t.Errorf("getBalanceHistory for an account with no entries = %d points, want 0", len(points))
	}
}

// TestGetBalanceHistory_AllRange exercises day-bucketing with from=nil: two
// distinct days, the second holding two same-day entries that must collapse
// into one bucket's delta rather than two points.
func TestGetBalanceHistory_AllRange(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	testAccountID := randomUUID(t)
	counterpartyAccountID := randomUUID(t)
	testLedgerID := insertLedgerAccount(t, ctx, pool, testAccountID)
	counterpartyLedgerID := insertLedgerAccount(t, ctx, pool, counterpartyAccountID)

	now := time.Now().UTC()
	day1 := now.AddDate(0, 0, -2)
	day2 := now.AddDate(0, 0, -1)

	insertEntryAt(t, ctx, pool, randomUUID(t), testLedgerID, 10000, day1)
	insertEntryAt(t, ctx, pool, randomUUID(t), counterpartyLedgerID, -10000, day1)

	// Two entries on day2 — must net into a single -1000 delta for that day.
	insertEntryAt(t, ctx, pool, randomUUID(t), testLedgerID, -3000, day2)
	insertEntryAt(t, ctx, pool, randomUUID(t), counterpartyLedgerID, 3000, day2)
	insertEntryAt(t, ctx, pool, randomUUID(t), testLedgerID, 2000, day2)
	insertEntryAt(t, ctx, pool, randomUUID(t), counterpartyLedgerID, -2000, day2)

	points, err := getBalanceHistory(ctx, pool, testAccountID, nil)
	if err != nil {
		t.Fatalf("getBalanceHistory: unexpected error: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("getBalanceHistory = %d points, want 2 (one per distinct day)", len(points))
	}
	if points[0].Balance != 10000 {
		t.Errorf("points[0].Balance = %d, want 10000 (day1's running total)", points[0].Balance)
	}
	if points[1].Balance != 9000 {
		t.Errorf("points[1].Balance = %d, want 9000 (day1's 10000 plus day2's net -1000)", points[1].Balance)
	}
	if !points[0].Date.Before(points[1].Date) {
		t.Errorf("points not ordered oldest-first: %v then %v", points[0].Date, points[1].Date)
	}
}

// TestGetBalanceHistory_BoundedRangeIncludesAnchor proves the from-set case:
// an entry before the window is folded into an anchor point dated at
// from (not surfaced as its own point), and an entry inside the window
// produces a second point building on that anchor.
func TestGetBalanceHistory_BoundedRangeIncludesAnchor(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	testAccountID := randomUUID(t)
	counterpartyAccountID := randomUUID(t)
	testLedgerID := insertLedgerAccount(t, ctx, pool, testAccountID)
	counterpartyLedgerID := insertLedgerAccount(t, ctx, pool, counterpartyAccountID)

	now := time.Now().UTC()
	before := now.AddDate(0, 0, -10) // outside the range
	inRange := now.AddDate(0, 0, -1) // inside the range

	insertEntryAt(t, ctx, pool, randomUUID(t), testLedgerID, 5000, before)
	insertEntryAt(t, ctx, pool, randomUUID(t), counterpartyLedgerID, -5000, before)

	insertEntryAt(t, ctx, pool, randomUUID(t), testLedgerID, 1500, inRange)
	insertEntryAt(t, ctx, pool, randomUUID(t), counterpartyLedgerID, -1500, inRange)

	from := now.AddDate(0, 0, -7) // excludes `before`, includes `inRange`

	points, err := getBalanceHistory(ctx, pool, testAccountID, &from)
	if err != nil {
		t.Fatalf("getBalanceHistory: unexpected error: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("getBalanceHistory = %d points, want 2 (anchor + the one in-range day)", len(points))
	}
	if points[0].Balance != 5000 {
		t.Errorf("points[0].Balance (anchor) = %d, want 5000 (balance carried into the range)", points[0].Balance)
	}
	wantAnchorDate := from.Truncate(24 * time.Hour)
	if !points[0].Date.Equal(wantAnchorDate) {
		t.Errorf("points[0].Date = %v, want %v (day-truncated from)", points[0].Date, wantAnchorDate)
	}
	if points[1].Balance != 6500 {
		t.Errorf("points[1].Balance = %d, want 6500 (5000 anchor plus the 1500 in-range entry)", points[1].Balance)
	}
}
