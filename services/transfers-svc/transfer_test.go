package main

import (
	"context"
	crand "crypto/rand"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountsv1 "neobank/proto/gen/go/accounts/v1"
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
