package main

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"os"
	"sync"
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
		_, outcome, err := executeTransfer(ctx, pool, accountID, counterpartyID, amount)
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
			_, outcome, err := executeTransfer(ctx, pool, fromAccountID, toAccountIDs[i], amount)
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
