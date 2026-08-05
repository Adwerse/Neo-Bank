package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	accountsv1 "neobank/proto/gen/go/accounts/v1"
)

func setDepositCreatedAtForTest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string, ts time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, "UPDATE deposits SET created_at = $1 WHERE id = $2", ts, id); err != nil {
		t.Fatalf("set deposit created_at: %v", err)
	}
}

func setWithdrawalCreatedAtForTest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string, ts time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, "UPDATE withdrawals SET created_at = $1 WHERE id = $2", ts, id); err != nil {
		t.Fatalf("set withdrawal created_at: %v", err)
	}
}

// insertWithdrawalForTest wraps the production insertWithdrawal (withdrawal.go)
// with test cleanup — there's no withdrawal-creation frontend flow yet
// (simulation-only), so unlike deposits/transfers there's no existing
// test-file equivalent to reuse.
func insertWithdrawalForTest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID string, amount int64) Withdrawal {
	t.Helper()
	w, err := insertWithdrawal(ctx, pool, accountID, amount, "payout_simulated", nil, nil)
	if err != nil {
		t.Fatalf("insert withdrawal: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM withdrawals WHERE id = $1", w.ID); err != nil {
			t.Logf("cleanup: delete withdrawal id=%s: %v", w.ID, err)
		}
	})
	return w
}

func TestGetDepositHistoryPage_CursorPagination(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	accountID := randomUUIDForTest(t)

	const total = 5
	base := time.Now().Add(-time.Hour)
	var ids []string
	for i := 0; i < total; i++ {
		id := insertDepositForReconcile(t, ctx, pool, accountID, int64(1000+i), "pending", nil)
		setDepositCreatedAtForTest(t, ctx, pool, id, base.Add(time.Duration(i)*time.Minute))
		ids = append(ids, id)
	}

	seen := map[string]bool{}
	var order []string
	var cursor *pageCursor
	for i := 0; i < total+1; i++ {
		page, hasMore, err := getDepositHistoryPage(ctx, pool, accountID, 2, cursor)
		if err != nil {
			t.Fatalf("getDepositHistoryPage: unexpected error: %v", err)
		}
		for _, d := range page {
			if seen[d.ID] {
				t.Fatalf("deposit %s returned more than once across pages", d.ID)
			}
			seen[d.ID] = true
			order = append(order, d.ID)
		}
		if !hasMore {
			break
		}
		last := page[len(page)-1]
		cursor = &pageCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}

	if len(seen) != total {
		t.Fatalf("saw %d distinct deposits across pages, want %d", len(seen), total)
	}
	for i, id := range order {
		want := ids[total-1-i] // newest first
		if id != want {
			t.Errorf("order[%d] = %s, want %s (created_at DESC, id DESC)", i, id, want)
		}
	}
}

func TestGetWithdrawalHistoryPage_CursorPagination(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	accountID := randomUUIDForTest(t)

	const total = 5
	base := time.Now().Add(-time.Hour)
	var ids []string
	for i := 0; i < total; i++ {
		w := insertWithdrawalForTest(t, ctx, pool, accountID, int64(1000+i))
		setWithdrawalCreatedAtForTest(t, ctx, pool, w.ID, base.Add(time.Duration(i)*time.Minute))
		ids = append(ids, w.ID)
	}

	seen := map[string]bool{}
	var order []string
	var cursor *pageCursor
	for i := 0; i < total+1; i++ {
		page, hasMore, err := getWithdrawalHistoryPage(ctx, pool, accountID, 2, cursor)
		if err != nil {
			t.Fatalf("getWithdrawalHistoryPage: unexpected error: %v", err)
		}
		for _, w := range page {
			if seen[w.ID] {
				t.Fatalf("withdrawal %s returned more than once across pages", w.ID)
			}
			seen[w.ID] = true
			order = append(order, w.ID)
		}
		if !hasMore {
			break
		}
		last := page[len(page)-1]
		cursor = &pageCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}

	if len(seen) != total {
		t.Fatalf("saw %d distinct withdrawals across pages, want %d", len(seen), total)
	}
	for i, id := range order {
		want := ids[total-1-i]
		if id != want {
			t.Errorf("order[%d] = %s, want %s (created_at DESC, id DESC)", i, id, want)
		}
	}
}

// TestGetOperationHistoryPage_MergesTypesInTimeOrder is the core regression
// guard for the unified feed: one transfer, one deposit, and one withdrawal
// on the same account, inserted out of chronological order, must come back
// strictly newest-first with the right Type tag on each — proving the
// three independently-queried, independently-sorted sources are correctly
// interleaved rather than just concatenated per-type.
func TestGetOperationHistoryPage_MergesTypesInTimeOrder(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	accountA := randomUUIDForTest(t)
	accountB := randomUUIDForTest(t)

	base := time.Now().Add(-time.Hour)

	transfer := insertPendingTransfer(t, ctx, pool, accountA, accountB, 1000)
	setTransferCreatedAtForTest(t, ctx, pool, transfer.ID, base) // oldest

	depositID := insertDepositForReconcile(t, ctx, pool, accountA, 2000, "pending", nil)
	setDepositCreatedAtForTest(t, ctx, pool, depositID, base.Add(1*time.Minute)) // middle

	withdrawal := insertWithdrawalForTest(t, ctx, pool, accountA, 3000)
	setWithdrawalCreatedAtForTest(t, ctx, pool, withdrawal.ID, base.Add(2*time.Minute)) // newest

	client := &fakeAccountsClient{
		resolveAccountsByIdsFunc: func(ctx context.Context, req *accountsv1.ResolveAccountsByIdsRequest) (*accountsv1.ResolveAccountsByIdsResponse, error) {
			return &accountsv1.ResolveAccountsByIdsResponse{
				Accounts: []*accountsv1.AccountSummary{{AccountId: accountB, AccountNumber: "NB0000000099"}},
			}, nil
		},
	}

	entries, hasMore, err := getOperationHistoryPage(ctx, pool, client, accountA, 20, nil)
	if err != nil {
		t.Fatalf("getOperationHistoryPage: unexpected error: %v", err)
	}
	if hasMore {
		t.Error("hasMore = true, want false (only 3 rows total, well under the limit)")
	}
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3", len(entries))
	}

	// Newest first: withdrawal, deposit, transfer.
	if entries[0].Type != "withdrawal" || entries[0].ID != withdrawal.ID {
		t.Errorf("entries[0] = {type:%s id:%s}, want {withdrawal %s}", entries[0].Type, entries[0].ID, withdrawal.ID)
	}
	if entries[1].Type != "deposit" || entries[1].ID != depositID {
		t.Errorf("entries[1] = {type:%s id:%s}, want {deposit %s}", entries[1].Type, entries[1].ID, depositID)
	}
	if entries[2].Type != "transfer" || entries[2].ID != transfer.ID {
		t.Errorf("entries[2] = {type:%s id:%s}, want {transfer %s}", entries[2].Type, entries[2].ID, transfer.ID)
	}
	if entries[2].Direction != "outgoing" {
		t.Errorf("entries[2].Direction = %q, want %q", entries[2].Direction, "outgoing")
	}
	if entries[2].CounterpartyAccountNumber != "NB0000000099" {
		t.Errorf("entries[2].CounterpartyAccountNumber = %q, want %q", entries[2].CounterpartyAccountNumber, "NB0000000099")
	}
}

// TestGetOperationHistoryPage_HasMoreReflectsEachBranch is the regression
// guard for the subtlety documented on getOperationHistoryPage: even when
// the combined candidate pool exactly fills limit (nothing trimmed), a
// single branch reporting its OWN hasMore=true must still surface as a
// global hasMore=true, because that branch has rows beyond what it
// contributed here that were never even fetched into the pool.
func TestGetOperationHistoryPage_HasMoreReflectsEachBranch(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	accountID := randomUUIDForTest(t)

	base := time.Now().Add(-time.Hour)
	// Three deposits, no transfers/withdrawals: with limit=2, the deposit
	// branch alone reports hasMore=true and contributes exactly 2 rows —
	// filling the pool exactly to the limit, not past it.
	for i := 0; i < 3; i++ {
		id := insertDepositForReconcile(t, ctx, pool, accountID, int64(1000+i), "pending", nil)
		setDepositCreatedAtForTest(t, ctx, pool, id, base.Add(time.Duration(i)*time.Minute))
	}

	client := &fakeAccountsClient{
		resolveAccountsByIdsFunc: func(ctx context.Context, req *accountsv1.ResolveAccountsByIdsRequest) (*accountsv1.ResolveAccountsByIdsResponse, error) {
			t.Fatal("ResolveAccountsByIds should not be called: this account has no transfers")
			return nil, nil
		},
	}

	entries, hasMore, err := getOperationHistoryPage(ctx, pool, client, accountID, 2, nil)
	if err != nil {
		t.Fatalf("getOperationHistoryPage: unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	if !hasMore {
		t.Error("hasMore = false, want true — the deposit branch alone has a 3rd row beyond this page, even though the merged pool didn't overflow limit")
	}
}
