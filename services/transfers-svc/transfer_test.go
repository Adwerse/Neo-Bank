package main

import (
	"context"
	crand "crypto/rand"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountsv1 "neobank/proto/gen/go/accounts/v1"
	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

// newTestPool connects to the postgres instance from docker-compose.yml via
// DATABASE_URL, skipping the test if it isn't set — matching ledger-svc's
// ledger_test.go convention for tests that exercise real SQL.
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

// randomUUIDForTest mints a UUID v4 by hand, matching ledger_test.go's
// randomUUID convention (this file can't call the production randomUUID
// directly for account ids since that one can fail on crypto/rand errors
// that would never happen in a test; a t.Fatalf-based variant is simpler
// for fixture data).
func randomUUIDForTest(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		t.Fatalf("generate random uuid: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// fakeAccountsClient implements accountsv1.AccountsServiceClient without a
// live accounts-svc, embedding the real interface as nil so only the two
// methods createTransfer actually calls need overriding — if the proto ever
// grows more RPCs, this fake keeps compiling instead of breaking every test.
type fakeAccountsClient struct {
	accountsv1.AccountsServiceClient
	resolveFunc func(ctx context.Context, req *accountsv1.ResolveAccountByNumberRequest) (*accountsv1.ResolveAccountByNumberResponse, error)
	getByIDFunc func(ctx context.Context, req *accountsv1.GetAccountByIDRequest) (*accountsv1.GetAccountByIDResponse, error)
}

func (f *fakeAccountsClient) ResolveAccountByNumber(ctx context.Context, req *accountsv1.ResolveAccountByNumberRequest, opts ...grpc.CallOption) (*accountsv1.ResolveAccountByNumberResponse, error) {
	return f.resolveFunc(ctx, req)
}

func (f *fakeAccountsClient) GetAccountByID(ctx context.Context, req *accountsv1.GetAccountByIDRequest, opts ...grpc.CallOption) (*accountsv1.GetAccountByIDResponse, error) {
	return f.getByIDFunc(ctx, req)
}

// fakeLedgerClient implements ledgerv1.LedgerServiceClient without a live
// ledger-svc, embedding the real interface as nil so only the one method
// settleTransfer actually calls needs overriding.
type fakeLedgerClient struct {
	ledgerv1.LedgerServiceClient
	executeTransferFunc func(ctx context.Context, req *ledgerv1.ExecuteTransferRequest) (*ledgerv1.ExecuteTransferResponse, error)
}

func (f *fakeLedgerClient) ExecuteTransfer(ctx context.Context, req *ledgerv1.ExecuteTransferRequest, opts ...grpc.CallOption) (*ledgerv1.ExecuteTransferResponse, error) {
	return f.executeTransferFunc(ctx, req)
}

// insertPendingTransfer inserts a transfers row directly (bypassing
// createTransfer/accountsClient entirely), for tests that are about
// settleTransfer, not validation.
func insertPendingTransfer(t *testing.T, ctx context.Context, pool *pgxpool.Pool, senderID, recipientID string, amount int64) Transfer {
	t.Helper()
	idempotencyKey := randomUUIDForTest(t)
	var tr Transfer
	err := pool.QueryRow(ctx,
		`INSERT INTO transfers (idempotency_key, sender_account_id, recipient_account_id, amount)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, idempotency_key, sender_account_id, recipient_account_id, amount, status, failure_reason, ledger_transaction_id, created_at, updated_at`,
		idempotencyKey, senderID, recipientID, amount,
	).Scan(&tr.ID, &tr.IdempotencyKey, &tr.SenderAccountID, &tr.RecipientAccountID, &tr.Amount, &tr.Status, &tr.FailureReason, &tr.LedgerTransactionID, &tr.CreatedAt, &tr.UpdatedAt)
	if err != nil {
		t.Fatalf("insert pending transfer: %v", err)
	}
	t.Cleanup(func() { deleteTransfer(t, ctx, pool, idempotencyKey) })
	return tr
}

// insertPendingTransferWithKey is like insertPendingTransfer but takes an
// explicit idempotency key instead of a random one, for tests that
// deliberately reuse the same key across multiple calls.
func insertPendingTransferWithKey(t *testing.T, ctx context.Context, pool *pgxpool.Pool, idempotencyKey, senderID, recipientID string, amount int64) Transfer {
	t.Helper()
	var tr Transfer
	err := pool.QueryRow(ctx,
		`INSERT INTO transfers (idempotency_key, sender_account_id, recipient_account_id, amount)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, idempotency_key, sender_account_id, recipient_account_id, amount, status, failure_reason, ledger_transaction_id, created_at, updated_at`,
		idempotencyKey, senderID, recipientID, amount,
	).Scan(&tr.ID, &tr.IdempotencyKey, &tr.SenderAccountID, &tr.RecipientAccountID, &tr.Amount, &tr.Status, &tr.FailureReason, &tr.LedgerTransactionID, &tr.CreatedAt, &tr.UpdatedAt)
	if err != nil {
		t.Fatalf("insert pending transfer: %v", err)
	}
	t.Cleanup(func() { deleteTransfer(t, ctx, pool, idempotencyKey) })
	return tr
}

// getTransferByID re-reads a transfers row, used to confirm settleTransfer's
// DB writes (or lack thereof, in the uncertain case).
func getTransferByID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string) Transfer {
	t.Helper()
	var tr Transfer
	err := pool.QueryRow(ctx,
		"SELECT id, idempotency_key, sender_account_id, recipient_account_id, amount, status, failure_reason, ledger_transaction_id, created_at, updated_at FROM transfers WHERE id = $1",
		id,
	).Scan(&tr.ID, &tr.IdempotencyKey, &tr.SenderAccountID, &tr.RecipientAccountID, &tr.Amount, &tr.Status, &tr.FailureReason, &tr.LedgerTransactionID, &tr.CreatedAt, &tr.UpdatedAt)
	if err != nil {
		t.Fatalf("get transfer id=%s: %v", id, err)
	}
	return tr
}

// transferCount returns how many transfers rows exist for idempotencyKey,
// used to assert that a rejected transfer wrote nothing at all.
func transferCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, idempotencyKey string) int {
	t.Helper()
	var count int
	err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM transfers WHERE idempotency_key = $1", idempotencyKey).Scan(&count)
	if err != nil {
		t.Fatalf("count transfers: %v", err)
	}
	return count
}

func deleteTransfer(t *testing.T, ctx context.Context, pool *pgxpool.Pool, idempotencyKey string) {
	t.Helper()
	if _, err := pool.Exec(ctx, "DELETE FROM transfers WHERE idempotency_key = $1", idempotencyKey); err != nil {
		t.Logf("cleanup: delete transfer idempotency_key=%s: %v", idempotencyKey, err)
	}
}

func TestCreateTransfer_Success(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	senderID := randomUUIDForTest(t)
	recipientID := randomUUIDForTest(t)
	idempotencyKey := randomUUIDForTest(t)
	t.Cleanup(func() { deleteTransfer(t, ctx, pool, idempotencyKey) })

	accountsClient := &fakeAccountsClient{
		resolveFunc: func(ctx context.Context, req *accountsv1.ResolveAccountByNumberRequest) (*accountsv1.ResolveAccountByNumberResponse, error) {
			return &accountsv1.ResolveAccountByNumberResponse{AccountId: recipientID, Status: accountStatusActive}, nil
		},
		getByIDFunc: func(ctx context.Context, req *accountsv1.GetAccountByIDRequest) (*accountsv1.GetAccountByIDResponse, error) {
			return &accountsv1.GetAccountByIDResponse{AccountId: senderID, Status: accountStatusActive}, nil
		},
	}

	transfer, outcome, err := createTransfer(ctx, pool, accountsClient, idempotencyKey, senderID, "NB0000000001", 1500)
	if err != nil {
		t.Fatalf("createTransfer: unexpected error: %v", err)
	}
	if outcome != createTransferOK {
		t.Fatalf("outcome = %v, want createTransferOK", outcome)
	}
	if transfer.Status != "pending" {
		t.Errorf("transfer.Status = %q, want \"pending\"", transfer.Status)
	}
	if transfer.SenderAccountID != senderID {
		t.Errorf("transfer.SenderAccountID = %q, want %q", transfer.SenderAccountID, senderID)
	}
	if transfer.RecipientAccountID != recipientID {
		t.Errorf("transfer.RecipientAccountID = %q, want %q", transfer.RecipientAccountID, recipientID)
	}
	if transfer.Amount != 1500 {
		t.Errorf("transfer.Amount = %d, want 1500", transfer.Amount)
	}
	if transfer.LedgerTransactionID != nil {
		t.Errorf("transfer.LedgerTransactionID = %v, want nil (no money has moved yet)", transfer.LedgerTransactionID)
	}

	if got := transferCount(t, ctx, pool, idempotencyKey); got != 1 {
		t.Errorf("transfers rows for idempotency_key=%s = %d, want 1", idempotencyKey, got)
	}
}

func TestCreateTransfer_InvalidAmount(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountsClient := &fakeAccountsClient{
		resolveFunc: func(ctx context.Context, req *accountsv1.ResolveAccountByNumberRequest) (*accountsv1.ResolveAccountByNumberResponse, error) {
			t.Fatal("ResolveAccountByNumber should not be called when amount is invalid")
			return nil, nil
		},
	}

	for _, amount := range []int64{0, -100} {
		idempotencyKey := randomUUIDForTest(t)
		t.Cleanup(func() { deleteTransfer(t, ctx, pool, idempotencyKey) })

		_, outcome, err := createTransfer(ctx, pool, accountsClient, idempotencyKey, randomUUIDForTest(t), "NB0000000001", amount)
		if err != nil {
			t.Fatalf("createTransfer(amount=%d): unexpected error: %v", amount, err)
		}
		if outcome != createTransferInvalidAmount {
			t.Errorf("createTransfer(amount=%d) outcome = %v, want createTransferInvalidAmount", amount, outcome)
		}
		if got := transferCount(t, ctx, pool, idempotencyKey); got != 0 {
			t.Errorf("transfers rows for amount=%d = %d, want 0", amount, got)
		}
	}
}

func TestCreateTransfer_RecipientNotFound(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	idempotencyKey := randomUUIDForTest(t)
	t.Cleanup(func() { deleteTransfer(t, ctx, pool, idempotencyKey) })

	accountsClient := &fakeAccountsClient{
		resolveFunc: func(ctx context.Context, req *accountsv1.ResolveAccountByNumberRequest) (*accountsv1.ResolveAccountByNumberResponse, error) {
			return nil, status.Error(codes.NotFound, "account not found")
		},
	}

	_, outcome, err := createTransfer(ctx, pool, accountsClient, idempotencyKey, randomUUIDForTest(t), "NB9999999999", 1000)
	if err != nil {
		t.Fatalf("createTransfer: unexpected error: %v", err)
	}
	if outcome != createTransferRecipientNotFound {
		t.Errorf("outcome = %v, want createTransferRecipientNotFound", outcome)
	}
	if got := transferCount(t, ctx, pool, idempotencyKey); got != 0 {
		t.Errorf("transfers rows = %d, want 0", got)
	}
}

func TestCreateTransfer_SelfTransfer(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	senderID := randomUUIDForTest(t)
	idempotencyKey := randomUUIDForTest(t)
	t.Cleanup(func() { deleteTransfer(t, ctx, pool, idempotencyKey) })

	accountsClient := &fakeAccountsClient{
		resolveFunc: func(ctx context.Context, req *accountsv1.ResolveAccountByNumberRequest) (*accountsv1.ResolveAccountByNumberResponse, error) {
			// The recipient's own account_number resolves back to the sender.
			return &accountsv1.ResolveAccountByNumberResponse{AccountId: senderID, Status: accountStatusActive}, nil
		},
	}

	_, outcome, err := createTransfer(ctx, pool, accountsClient, idempotencyKey, senderID, "NB0000000002", 1000)
	if err != nil {
		t.Fatalf("createTransfer: unexpected error: %v", err)
	}
	if outcome != createTransferSelfTransfer {
		t.Errorf("outcome = %v, want createTransferSelfTransfer", outcome)
	}
	if got := transferCount(t, ctx, pool, idempotencyKey); got != 0 {
		t.Errorf("transfers rows = %d, want 0", got)
	}
}

func TestCreateTransfer_RecipientClosed(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	senderID := randomUUIDForTest(t)
	recipientID := randomUUIDForTest(t)
	idempotencyKey := randomUUIDForTest(t)
	t.Cleanup(func() { deleteTransfer(t, ctx, pool, idempotencyKey) })

	accountsClient := &fakeAccountsClient{
		resolveFunc: func(ctx context.Context, req *accountsv1.ResolveAccountByNumberRequest) (*accountsv1.ResolveAccountByNumberResponse, error) {
			return &accountsv1.ResolveAccountByNumberResponse{AccountId: recipientID, Status: accountStatusClosed}, nil
		},
	}

	_, outcome, err := createTransfer(ctx, pool, accountsClient, idempotencyKey, senderID, "NB0000000003", 1000)
	if err != nil {
		t.Fatalf("createTransfer: unexpected error: %v", err)
	}
	if outcome != createTransferRecipientClosed {
		t.Errorf("outcome = %v, want createTransferRecipientClosed", outcome)
	}
	if got := transferCount(t, ctx, pool, idempotencyKey); got != 0 {
		t.Errorf("transfers rows = %d, want 0", got)
	}
}

func TestCreateTransfer_SenderNotActive(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	senderID := randomUUIDForTest(t)
	recipientID := randomUUIDForTest(t)
	idempotencyKey := randomUUIDForTest(t)
	t.Cleanup(func() { deleteTransfer(t, ctx, pool, idempotencyKey) })

	accountsClient := &fakeAccountsClient{
		resolveFunc: func(ctx context.Context, req *accountsv1.ResolveAccountByNumberRequest) (*accountsv1.ResolveAccountByNumberResponse, error) {
			return &accountsv1.ResolveAccountByNumberResponse{AccountId: recipientID, Status: accountStatusActive}, nil
		},
		getByIDFunc: func(ctx context.Context, req *accountsv1.GetAccountByIDRequest) (*accountsv1.GetAccountByIDResponse, error) {
			return &accountsv1.GetAccountByIDResponse{AccountId: senderID, Status: "frozen"}, nil
		},
	}

	_, outcome, err := createTransfer(ctx, pool, accountsClient, idempotencyKey, senderID, "NB0000000004", 1000)
	if err != nil {
		t.Fatalf("createTransfer: unexpected error: %v", err)
	}
	if outcome != createTransferSenderNotActive {
		t.Errorf("outcome = %v, want createTransferSenderNotActive", outcome)
	}
	if got := transferCount(t, ctx, pool, idempotencyKey); got != 0 {
		t.Errorf("transfers rows = %d, want 0", got)
	}
}

func TestSettleTransfer_Success(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	pending := insertPendingTransfer(t, ctx, pool, randomUUIDForTest(t), randomUUIDForTest(t), 1500)
	wantTransactionID := randomUUIDForTest(t)

	ledgerClient := &fakeLedgerClient{
		executeTransferFunc: func(ctx context.Context, req *ledgerv1.ExecuteTransferRequest) (*ledgerv1.ExecuteTransferResponse, error) {
			if req.GetFromAccountId() != pending.SenderAccountID || req.GetToAccountId() != pending.RecipientAccountID || req.GetAmount() != pending.Amount {
				t.Errorf("ExecuteTransfer request = %+v, want from=%s to=%s amount=%d", req, pending.SenderAccountID, pending.RecipientAccountID, pending.Amount)
			}
			return &ledgerv1.ExecuteTransferResponse{TransactionId: wantTransactionID}, nil
		},
	}

	settled, outcome, err := settleTransfer(ctx, pool, ledgerClient, pending)
	if err != nil {
		t.Fatalf("settleTransfer: unexpected error: %v", err)
	}
	if outcome != settlementCompleted {
		t.Fatalf("outcome = %v, want settlementCompleted", outcome)
	}
	if settled.Status != "completed" {
		t.Errorf("settled.Status = %q, want \"completed\"", settled.Status)
	}
	if settled.LedgerTransactionID == nil || *settled.LedgerTransactionID != wantTransactionID {
		t.Errorf("settled.LedgerTransactionID = %v, want %q", settled.LedgerTransactionID, wantTransactionID)
	}

	row := getTransferByID(t, ctx, pool, pending.ID)
	if row.Status != "completed" {
		t.Errorf("row.Status = %q, want \"completed\"", row.Status)
	}
}

func TestSettleTransfer_InsufficientFunds(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	pending := insertPendingTransfer(t, ctx, pool, randomUUIDForTest(t), randomUUIDForTest(t), 5000)

	ledgerClient := &fakeLedgerClient{
		executeTransferFunc: func(ctx context.Context, req *ledgerv1.ExecuteTransferRequest) (*ledgerv1.ExecuteTransferResponse, error) {
			return nil, status.Error(codes.FailedPrecondition, "insufficient funds")
		},
	}

	settled, outcome, err := settleTransfer(ctx, pool, ledgerClient, pending)
	if err != nil {
		t.Fatalf("settleTransfer: unexpected error: %v", err)
	}
	if outcome != settlementFailed {
		t.Fatalf("outcome = %v, want settlementFailed", outcome)
	}
	if settled.Status != "failed" {
		t.Errorf("settled.Status = %q, want \"failed\"", settled.Status)
	}
	if settled.FailureReason == nil || *settled.FailureReason != failureReasonInsufficientFunds {
		t.Errorf("settled.FailureReason = %v, want %q", settled.FailureReason, failureReasonInsufficientFunds)
	}

	row := getTransferByID(t, ctx, pool, pending.ID)
	if row.Status != "failed" {
		t.Errorf("row.Status = %q, want \"failed\"", row.Status)
	}
}

func TestSettleTransfer_AccountNotFound(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	pending := insertPendingTransfer(t, ctx, pool, randomUUIDForTest(t), randomUUIDForTest(t), 1000)

	ledgerClient := &fakeLedgerClient{
		executeTransferFunc: func(ctx context.Context, req *ledgerv1.ExecuteTransferRequest) (*ledgerv1.ExecuteTransferResponse, error) {
			return nil, status.Error(codes.NotFound, "from account not found")
		},
	}

	settled, outcome, err := settleTransfer(ctx, pool, ledgerClient, pending)
	if err != nil {
		t.Fatalf("settleTransfer: unexpected error: %v", err)
	}
	if outcome != settlementFailed {
		t.Fatalf("outcome = %v, want settlementFailed", outcome)
	}
	if settled.FailureReason == nil || *settled.FailureReason != failureReasonAccountNotFound {
		t.Errorf("settled.FailureReason = %v, want %q", settled.FailureReason, failureReasonAccountNotFound)
	}
}

func TestSettleTransfer_Uncertain(t *testing.T) {
	for _, code := range []codes.Code{codes.Unavailable, codes.DeadlineExceeded} {
		t.Run(code.String(), func(t *testing.T) {
			pool := newTestPool(t)
			ctx := context.Background()

			pending := insertPendingTransfer(t, ctx, pool, randomUUIDForTest(t), randomUUIDForTest(t), 1000)

			ledgerClient := &fakeLedgerClient{
				executeTransferFunc: func(ctx context.Context, req *ledgerv1.ExecuteTransferRequest) (*ledgerv1.ExecuteTransferResponse, error) {
					return nil, status.Error(code, "simulated transport failure")
				},
			}

			settled, outcome, err := settleTransfer(ctx, pool, ledgerClient, pending)
			if err != nil {
				t.Fatalf("settleTransfer: unexpected error: %v", err)
			}
			if outcome != settlementUncertain {
				t.Fatalf("outcome = %v, want settlementUncertain", outcome)
			}
			if settled.Status != "pending" {
				t.Errorf("settled.Status = %q, want \"pending\" (unchanged)", settled.Status)
			}

			row := getTransferByID(t, ctx, pool, pending.ID)
			if row.Status != "pending" {
				t.Errorf("row.Status = %q, want \"pending\" (settleTransfer must not write anything when uncertain)", row.Status)
			}
			if row.LedgerTransactionID != nil {
				t.Errorf("row.LedgerTransactionID = %v, want nil", row.LedgerTransactionID)
			}
			if row.FailureReason != nil {
				t.Errorf("row.FailureReason = %v, want nil", row.FailureReason)
			}
		})
	}
}

func TestCreateTransfer_IdempotentReplay_SameParams(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	senderID := randomUUIDForTest(t)
	recipientID := randomUUIDForTest(t)
	idempotencyKey := randomUUIDForTest(t)
	t.Cleanup(func() { deleteTransfer(t, ctx, pool, idempotencyKey) })

	accountsClient := &fakeAccountsClient{
		resolveFunc: func(ctx context.Context, req *accountsv1.ResolveAccountByNumberRequest) (*accountsv1.ResolveAccountByNumberResponse, error) {
			return &accountsv1.ResolveAccountByNumberResponse{AccountId: recipientID, Status: accountStatusActive}, nil
		},
		getByIDFunc: func(ctx context.Context, req *accountsv1.GetAccountByIDRequest) (*accountsv1.GetAccountByIDResponse, error) {
			return &accountsv1.GetAccountByIDResponse{AccountId: senderID, Status: accountStatusActive}, nil
		},
	}

	first, outcome1, err := createTransfer(ctx, pool, accountsClient, idempotencyKey, senderID, "NB0000000010", 2000)
	if err != nil {
		t.Fatalf("createTransfer (first): unexpected error: %v", err)
	}
	if outcome1 != createTransferOK {
		t.Fatalf("first outcome = %v, want createTransferOK", outcome1)
	}

	second, outcome2, err := createTransfer(ctx, pool, accountsClient, idempotencyKey, senderID, "NB0000000010", 2000)
	if err != nil {
		t.Fatalf("createTransfer (second): unexpected error: %v", err)
	}
	if outcome2 != createTransferReplayed {
		t.Fatalf("second outcome = %v, want createTransferReplayed", outcome2)
	}
	if second.ID != first.ID {
		t.Errorf("second.ID = %q, want %q (same transfer)", second.ID, first.ID)
	}

	if got := transferCount(t, ctx, pool, idempotencyKey); got != 1 {
		t.Errorf("transfers rows = %d, want 1", got)
	}
}

func TestCreateTransfer_IdempotentReplay_DifferentParams(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	senderID := randomUUIDForTest(t)
	recipientID := randomUUIDForTest(t)
	idempotencyKey := randomUUIDForTest(t)
	t.Cleanup(func() { deleteTransfer(t, ctx, pool, idempotencyKey) })

	accountsClient := &fakeAccountsClient{
		resolveFunc: func(ctx context.Context, req *accountsv1.ResolveAccountByNumberRequest) (*accountsv1.ResolveAccountByNumberResponse, error) {
			return &accountsv1.ResolveAccountByNumberResponse{AccountId: recipientID, Status: accountStatusActive}, nil
		},
		getByIDFunc: func(ctx context.Context, req *accountsv1.GetAccountByIDRequest) (*accountsv1.GetAccountByIDResponse, error) {
			return &accountsv1.GetAccountByIDResponse{AccountId: senderID, Status: accountStatusActive}, nil
		},
	}

	_, outcome1, err := createTransfer(ctx, pool, accountsClient, idempotencyKey, senderID, "NB0000000011", 2000)
	if err != nil {
		t.Fatalf("createTransfer (first): unexpected error: %v", err)
	}
	if outcome1 != createTransferOK {
		t.Fatalf("first outcome = %v, want createTransferOK", outcome1)
	}

	// Same key, different amount.
	_, outcome2, err := createTransfer(ctx, pool, accountsClient, idempotencyKey, senderID, "NB0000000011", 9999)
	if err != nil {
		t.Fatalf("createTransfer (reused key, different amount): unexpected error: %v", err)
	}
	if outcome2 != createTransferKeyReused {
		t.Errorf("outcome = %v, want createTransferKeyReused (different amount)", outcome2)
	}

	// Same key, different sender.
	_, outcome3, err := createTransfer(ctx, pool, accountsClient, idempotencyKey, randomUUIDForTest(t), "NB0000000011", 2000)
	if err != nil {
		t.Fatalf("createTransfer (reused key, different sender): unexpected error: %v", err)
	}
	if outcome3 != createTransferKeyReused {
		t.Errorf("outcome = %v, want createTransferKeyReused (different sender)", outcome3)
	}

	if got := transferCount(t, ctx, pool, idempotencyKey); got != 1 {
		t.Errorf("transfers rows = %d, want 1 (mismatched retries must not create rows)", got)
	}
}

func TestCreateTransfer_ReplayReturnsCurrentState(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	senderID := randomUUIDForTest(t)
	recipientID := randomUUIDForTest(t)
	idempotencyKey := randomUUIDForTest(t)

	pending := insertPendingTransferWithKey(t, ctx, pool, idempotencyKey, senderID, recipientID, 3000)
	if _, err := markTransferCompleted(ctx, pool, pending.ID, randomUUIDForTest(t)); err != nil {
		t.Fatalf("markTransferCompleted: %v", err)
	}

	accountsClient := &fakeAccountsClient{
		resolveFunc: func(ctx context.Context, req *accountsv1.ResolveAccountByNumberRequest) (*accountsv1.ResolveAccountByNumberResponse, error) {
			return &accountsv1.ResolveAccountByNumberResponse{AccountId: recipientID, Status: accountStatusActive}, nil
		},
	}

	replayed, outcome, err := createTransfer(ctx, pool, accountsClient, idempotencyKey, senderID, "NB0000000012", 3000)
	if err != nil {
		t.Fatalf("createTransfer: unexpected error: %v", err)
	}
	if outcome != createTransferReplayed {
		t.Fatalf("outcome = %v, want createTransferReplayed", outcome)
	}
	if replayed.Status != "completed" {
		t.Errorf("replayed.Status = %q, want \"completed\" (must reflect current settled state, not a stale pending snapshot)", replayed.Status)
	}

	if got := transferCount(t, ctx, pool, idempotencyKey); got != 1 {
		t.Errorf("transfers rows = %d, want 1", got)
	}
}

// TestCreateTransfer_ConcurrentDuplicate is the direct proof for this
// sprint's core DoD item, mirroring ledger-svc's
// TestExecuteTransfer_ConcurrentOverdraftPrevention concurrency-test style
// (services/ledger-svc/ledger_test.go): it fires goroutines concurrently
// instead of calling createTransfer sequentially, since only genuine
// concurrent access exercises the race the UNIQUE constraint is meant to
// catch — a sequential call sequence would pass even without the
// unique-violation handling in createTransfer at all.
func TestCreateTransfer_ConcurrentDuplicate(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	senderID := randomUUIDForTest(t)
	recipientID := randomUUIDForTest(t)
	idempotencyKey := randomUUIDForTest(t)
	t.Cleanup(func() { deleteTransfer(t, ctx, pool, idempotencyKey) })

	accountsClient := &fakeAccountsClient{
		resolveFunc: func(ctx context.Context, req *accountsv1.ResolveAccountByNumberRequest) (*accountsv1.ResolveAccountByNumberResponse, error) {
			return &accountsv1.ResolveAccountByNumberResponse{AccountId: recipientID, Status: accountStatusActive}, nil
		},
		getByIDFunc: func(ctx context.Context, req *accountsv1.GetAccountByIDRequest) (*accountsv1.GetAccountByIDResponse, error) {
			return &accountsv1.GetAccountByIDResponse{AccountId: senderID, Status: accountStatusActive}, nil
		},
	}

	const attempts = 20
	outcomes := make([]createTransferOutcome, attempts)
	ids := make([]string, attempts)
	errs := make([]error, attempts)

	var wg sync.WaitGroup
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func(i int) {
			defer wg.Done()
			transfer, outcome, err := createTransfer(ctx, pool, accountsClient, idempotencyKey, senderID, "NB0000000013", 2500)
			outcomes[i] = outcome
			ids[i] = transfer.ID
			errs[i] = err
		}(i)
	}
	wg.Wait()

	var created, replayed int
	var wonID string
	for i, outcome := range outcomes {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: unexpected error: %v", i, errs[i])
		}
		switch outcome {
		case createTransferOK:
			created++
			wonID = ids[i]
		case createTransferReplayed:
			replayed++
		default:
			t.Fatalf("goroutine %d: unexpected outcome %v", i, outcome)
		}
	}
	if created != 1 {
		t.Errorf("created = %d, want exactly 1", created)
	}
	if replayed != attempts-1 {
		t.Errorf("replayed = %d, want %d", replayed, attempts-1)
	}
	for i, id := range ids {
		if id != wonID {
			t.Errorf("goroutine %d: transfer id = %q, want %q (same transfer as the winner)", i, id, wonID)
		}
	}

	if got := transferCount(t, ctx, pool, idempotencyKey); got != 1 {
		t.Errorf("transfers rows = %d, want exactly 1", got)
	}
}

func TestGetTransfersForAccount(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountA := randomUUIDForTest(t)
	accountB := randomUUIDForTest(t)
	accountC := randomUUIDForTest(t)

	sent := insertPendingTransfer(t, ctx, pool, accountA, accountB, 1000)
	received := insertPendingTransfer(t, ctx, pool, accountB, accountA, 2000)
	unrelated := insertPendingTransfer(t, ctx, pool, accountB, accountC, 3000)

	transfers, err := getTransfersForAccount(ctx, pool, accountA, 20, 0)
	if err != nil {
		t.Fatalf("getTransfersForAccount: unexpected error: %v", err)
	}

	ids := make(map[string]bool, len(transfers))
	for _, tr := range transfers {
		ids[tr.ID] = true
	}
	if !ids[sent.ID] {
		t.Errorf("expected sent transfer %s in results", sent.ID)
	}
	if !ids[received.ID] {
		t.Errorf("expected received transfer %s in results", received.ID)
	}
	if ids[unrelated.ID] {
		t.Errorf("unrelated transfer %s should not be in accountA's results", unrelated.ID)
	}

	// Pagination: limit=1 should return exactly one row.
	limited, err := getTransfersForAccount(ctx, pool, accountA, 1, 0)
	if err != nil {
		t.Fatalf("getTransfersForAccount (limit=1): unexpected error: %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("len(limited) = %d, want 1", len(limited))
	}
}
