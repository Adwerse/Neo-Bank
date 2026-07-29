package main

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountsv1 "neobank/proto/gen/go/accounts/v1"
	fraudv1 "neobank/proto/gen/go/fraud/v1"
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
// live accounts-svc, embedding the real interface as nil so only the
// methods actually exercised need overriding — if the proto ever grows more
// RPCs, this fake keeps compiling instead of breaking every test.
type fakeAccountsClient struct {
	accountsv1.AccountsServiceClient
	resolveFunc              func(ctx context.Context, req *accountsv1.ResolveAccountByNumberRequest) (*accountsv1.ResolveAccountByNumberResponse, error)
	getByIDFunc              func(ctx context.Context, req *accountsv1.GetAccountByIDRequest) (*accountsv1.GetAccountByIDResponse, error)
	getByUserIDFunc          func(ctx context.Context, req *accountsv1.GetAccountByUserIDRequest) (*accountsv1.GetAccountByUserIDResponse, error)
	resolveAccountsByIdsFunc func(ctx context.Context, req *accountsv1.ResolveAccountsByIdsRequest) (*accountsv1.ResolveAccountsByIdsResponse, error)
}

func (f *fakeAccountsClient) ResolveAccountByNumber(ctx context.Context, req *accountsv1.ResolveAccountByNumberRequest, opts ...grpc.CallOption) (*accountsv1.ResolveAccountByNumberResponse, error) {
	return f.resolveFunc(ctx, req)
}

func (f *fakeAccountsClient) GetAccountByID(ctx context.Context, req *accountsv1.GetAccountByIDRequest, opts ...grpc.CallOption) (*accountsv1.GetAccountByIDResponse, error) {
	return f.getByIDFunc(ctx, req)
}

func (f *fakeAccountsClient) GetAccountByUserID(ctx context.Context, req *accountsv1.GetAccountByUserIDRequest, opts ...grpc.CallOption) (*accountsv1.GetAccountByUserIDResponse, error) {
	return f.getByUserIDFunc(ctx, req)
}

func (f *fakeAccountsClient) ResolveAccountsByIds(ctx context.Context, req *accountsv1.ResolveAccountsByIdsRequest, opts ...grpc.CallOption) (*accountsv1.ResolveAccountsByIdsResponse, error) {
	return f.resolveAccountsByIdsFunc(ctx, req)
}

// fakeLedgerClient implements ledgerv1.LedgerServiceClient without a live
// ledger-svc, embedding the real interface as nil so only the one method
// settleTransfer actually calls needs overriding.
type fakeLedgerClient struct {
	ledgerv1.LedgerServiceClient
	executeTransferFunc           func(ctx context.Context, req *ledgerv1.ExecuteTransferRequest) (*ledgerv1.ExecuteTransferResponse, error)
	getTransactionByReferenceFunc func(ctx context.Context, req *ledgerv1.GetTransactionByReferenceRequest) (*ledgerv1.GetTransactionByReferenceResponse, error)
}

func (f *fakeLedgerClient) ExecuteTransfer(ctx context.Context, req *ledgerv1.ExecuteTransferRequest, opts ...grpc.CallOption) (*ledgerv1.ExecuteTransferResponse, error) {
	return f.executeTransferFunc(ctx, req)
}

func (f *fakeLedgerClient) GetTransactionByReference(ctx context.Context, req *ledgerv1.GetTransactionByReferenceRequest, opts ...grpc.CallOption) (*ledgerv1.GetTransactionByReferenceResponse, error) {
	return f.getTransactionByReferenceFunc(ctx, req)
}

// fakeFraudClient implements fraudv1.FraudServiceClient without a live
// fraud-svc, embedding the real interface as nil so only the one method
// checkTransferFraud actually calls needs overriding.
type fakeFraudClient struct {
	fraudv1.FraudServiceClient
	checkTransferFunc func(ctx context.Context, req *fraudv1.CheckTransferRequest) (*fraudv1.CheckTransferResponse, error)
}

func (f *fakeFraudClient) CheckTransfer(ctx context.Context, req *fraudv1.CheckTransferRequest, opts ...grpc.CallOption) (*fraudv1.CheckTransferResponse, error) {
	return f.checkTransferFunc(ctx, req)
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

// setTransferCreatedAtForTest overrides a transfer's created_at directly,
// for pagination tests that need deterministic, distinctly-ordered rows
// rather than relying on real wall-clock spacing between inserts.
func setTransferCreatedAtForTest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id string, ts time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, "UPDATE transfers SET created_at = $1 WHERE id = $2", ts, id); err != nil {
		t.Fatalf("set transfer created_at: %v", err)
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

func TestCheckTransferFraud_Approved(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	pending := insertPendingTransfer(t, ctx, pool, randomUUIDForTest(t), randomUUIDForTest(t), 1000)

	fraudClient := &fakeFraudClient{
		checkTransferFunc: func(ctx context.Context, req *fraudv1.CheckTransferRequest) (*fraudv1.CheckTransferResponse, error) {
			if req.GetTransferId() != pending.ID || req.GetAccountId() != pending.SenderAccountID || req.GetAmount() != pending.Amount {
				t.Errorf("CheckTransfer request = %+v, want transfer_id=%s account_id=%s amount=%d", req, pending.ID, pending.SenderAccountID, pending.Amount)
			}
			return &fraudv1.CheckTransferResponse{Decision: "approve", Reason: "no rule triggered"}, nil
		},
	}

	checked, outcome, err := checkTransferFraud(ctx, pool, fraudClient, pending)
	if err != nil {
		t.Fatalf("checkTransferFraud: unexpected error: %v", err)
	}
	if outcome != fraudCheckApproved {
		t.Fatalf("outcome = %v, want fraudCheckApproved", outcome)
	}
	if checked.Status != "pending" {
		t.Errorf("checked.Status = %q, want \"pending\" (unchanged — settleTransfer writes the terminal state)", checked.Status)
	}

	row := getTransferByID(t, ctx, pool, pending.ID)
	if row.Status != "pending" {
		t.Errorf("row.Status = %q, want \"pending\" (checkTransferFraud must not write anything on approve)", row.Status)
	}
}

func TestCheckTransferFraud_Rejected(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	pending := insertPendingTransfer(t, ctx, pool, randomUUIDForTest(t), randomUUIDForTest(t), 600000)

	fraudClient := &fakeFraudClient{
		checkTransferFunc: func(ctx context.Context, req *fraudv1.CheckTransferRequest) (*fraudv1.CheckTransferResponse, error) {
			return &fraudv1.CheckTransferResponse{
				Decision:      "reject",
				TriggeredRule: "amount_threshold",
				Reason:        "amount_threshold: observed 600000 exceeds threshold 500000",
			}, nil
		},
	}

	checked, outcome, err := checkTransferFraud(ctx, pool, fraudClient, pending)
	if err != nil {
		t.Fatalf("checkTransferFraud: unexpected error: %v", err)
	}
	if outcome != fraudCheckRejected {
		t.Fatalf("outcome = %v, want fraudCheckRejected", outcome)
	}
	if checked.Status != "rejected" {
		t.Errorf("checked.Status = %q, want \"rejected\"", checked.Status)
	}
	if checked.FailureReason == nil || *checked.FailureReason != "amount_threshold" {
		t.Errorf("checked.FailureReason = %v, want \"amount_threshold\"", checked.FailureReason)
	}

	row := getTransferByID(t, ctx, pool, pending.ID)
	if row.Status != "rejected" {
		t.Errorf("row.Status = %q, want \"rejected\"", row.Status)
	}
	if row.FailureReason == nil || *row.FailureReason != "amount_threshold" {
		t.Errorf("row.FailureReason = %v, want \"amount_threshold\"", row.FailureReason)
	}
	if row.LedgerTransactionID != nil {
		t.Errorf("row.LedgerTransactionID = %v, want nil (ledger must never be called on a fraud reject)", row.LedgerTransactionID)
	}
}

func TestCheckTransferFraud_Uncertain(t *testing.T) {
	for _, code := range []codes.Code{codes.Internal, codes.Unavailable} {
		t.Run(code.String(), func(t *testing.T) {
			pool := newTestPool(t)
			ctx := context.Background()

			pending := insertPendingTransfer(t, ctx, pool, randomUUIDForTest(t), randomUUIDForTest(t), 1000)

			fraudClient := &fakeFraudClient{
				checkTransferFunc: func(ctx context.Context, req *fraudv1.CheckTransferRequest) (*fraudv1.CheckTransferResponse, error) {
					return nil, status.Error(code, "simulated fraud-svc failure")
				},
			}

			checked, outcome, err := checkTransferFraud(ctx, pool, fraudClient, pending)
			if err != nil {
				t.Fatalf("checkTransferFraud: unexpected error: %v", err)
			}
			if outcome != fraudCheckUncertain {
				t.Fatalf("outcome = %v, want fraudCheckUncertain", outcome)
			}
			if checked.Status != "pending" {
				t.Errorf("checked.Status = %q, want \"pending\" (unchanged)", checked.Status)
			}

			row := getTransferByID(t, ctx, pool, pending.ID)
			if row.Status != "pending" {
				t.Errorf("row.Status = %q, want \"pending\" (checkTransferFraud must not write anything when uncertain)", row.Status)
			}
			if row.FailureReason != nil {
				t.Errorf("row.FailureReason = %v, want nil", row.FailureReason)
			}
		})
	}
}

func TestCheckTransferFraud_UnexpectedDecision(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	pending := insertPendingTransfer(t, ctx, pool, randomUUIDForTest(t), randomUUIDForTest(t), 1000)

	fraudClient := &fakeFraudClient{
		checkTransferFunc: func(ctx context.Context, req *fraudv1.CheckTransferRequest) (*fraudv1.CheckTransferResponse, error) {
			return &fraudv1.CheckTransferResponse{Decision: ""}, nil
		},
	}

	checked, outcome, err := checkTransferFraud(ctx, pool, fraudClient, pending)
	if err != nil {
		t.Fatalf("checkTransferFraud: unexpected error: %v", err)
	}
	if outcome != fraudCheckUncertain {
		t.Fatalf("outcome = %v, want fraudCheckUncertain (fail closed on an unrecognized decision)", outcome)
	}
	if checked.Status != "pending" {
		t.Errorf("checked.Status = %q, want \"pending\" (unchanged)", checked.Status)
	}

	row := getTransferByID(t, ctx, pool, pending.ID)
	if row.Status != "pending" {
		t.Errorf("row.Status = %q, want \"pending\" (must not write anything on an unrecognized decision)", row.Status)
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

func TestGetTransferHistoryPage_BothDirectionsAndVisibility(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountA := randomUUIDForTest(t)
	accountB := randomUUIDForTest(t)
	accountC := randomUUIDForTest(t)

	outgoing := insertPendingTransfer(t, ctx, pool, accountA, accountB, 1000)
	incoming := insertPendingTransfer(t, ctx, pool, accountB, accountA, 2000)
	if _, err := markTransferCompleted(ctx, pool, incoming.ID, randomUUIDForTest(t)); err != nil {
		t.Fatalf("markTransferCompleted: %v", err)
	}
	unrelated := insertPendingTransfer(t, ctx, pool, accountB, accountC, 3000)

	transfers, hasMore, err := getTransferHistoryPage(ctx, pool, accountA, 20, nil)
	if err != nil {
		t.Fatalf("getTransferHistoryPage: unexpected error: %v", err)
	}
	if hasMore {
		t.Errorf("hasMore = true, want false (only 2 rows for a limit of 20)")
	}

	ids := make(map[string]bool, len(transfers))
	for _, tr := range transfers {
		ids[tr.ID] = true
	}
	if !ids[outgoing.ID] {
		t.Errorf("expected outgoing (pending) transfer %s in accountA's results", outgoing.ID)
	}
	if !ids[incoming.ID] {
		t.Errorf("expected incoming (completed) transfer %s in accountA's results", incoming.ID)
	}
	if ids[unrelated.ID] {
		t.Errorf("unrelated transfer %s should not be in accountA's results", unrelated.ID)
	}

	// Pagination: limit=1 should return exactly one row and report more.
	limited, hasMore, err := getTransferHistoryPage(ctx, pool, accountA, 1, nil)
	if err != nil {
		t.Fatalf("getTransferHistoryPage (limit=1): unexpected error: %v", err)
	}
	if len(limited) != 1 {
		t.Errorf("len(limited) = %d, want 1", len(limited))
	}
	if !hasMore {
		t.Errorf("hasMore = false, want true (2 rows exist for a limit of 1)")
	}
}

func TestGetTransferHistoryPage_RecipientHidesNonCompleted(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountA := randomUUIDForTest(t)
	accountB := randomUUIDForTest(t)

	pending := insertPendingTransfer(t, ctx, pool, accountB, accountA, 1000)

	failed := insertPendingTransfer(t, ctx, pool, accountB, accountA, 2000)
	if _, err := markTransferFailed(ctx, pool, failed.ID, failureReasonInsufficientFunds); err != nil {
		t.Fatalf("markTransferFailed: %v", err)
	}

	rejected := insertPendingTransfer(t, ctx, pool, accountB, accountA, 3000)
	if _, err := markTransferRejected(ctx, pool, rejected.ID, "amount_threshold"); err != nil {
		t.Fatalf("markTransferRejected: %v", err)
	}

	// Recipient (accountA) should see none of these three.
	recipientView, _, err := getTransferHistoryPage(ctx, pool, accountA, 20, nil)
	if err != nil {
		t.Fatalf("getTransferHistoryPage(accountA): unexpected error: %v", err)
	}
	for _, tr := range recipientView {
		if tr.ID == pending.ID || tr.ID == failed.ID || tr.ID == rejected.ID {
			t.Errorf("recipient accountA should not see transfer %s (status=%s)", tr.ID, tr.Status)
		}
	}

	// Sender (accountB) should see all three, in every status.
	senderView, _, err := getTransferHistoryPage(ctx, pool, accountB, 20, nil)
	if err != nil {
		t.Fatalf("getTransferHistoryPage(accountB): unexpected error: %v", err)
	}
	senderIDs := make(map[string]bool, len(senderView))
	for _, tr := range senderView {
		senderIDs[tr.ID] = true
	}
	for _, tr := range []Transfer{pending, failed, rejected} {
		if !senderIDs[tr.ID] {
			t.Errorf("sender accountB should see its own transfer %s", tr.ID)
		}
	}
}

func TestGetTransferHistoryPage_CursorPagination(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountA := randomUUIDForTest(t)
	accountB := randomUUIDForTest(t)

	const total = 5
	base := time.Now().Add(-time.Hour)
	inserted := make([]Transfer, total)
	for i := 0; i < total; i++ {
		tr := insertPendingTransfer(t, ctx, pool, accountA, accountB, int64(1000+i))
		setTransferCreatedAtForTest(t, ctx, pool, tr.ID, base.Add(time.Duration(i)*time.Minute))
		tr.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		inserted[i] = tr
	}

	seen := map[string]bool{}
	var order []string
	var cursor *pageCursor
	for i := 0; i < total+1; i++ { // +1 guards against an infinite loop on a bug
		page, hasMore, err := getTransferHistoryPage(ctx, pool, accountA, 2, cursor)
		if err != nil {
			t.Fatalf("getTransferHistoryPage: unexpected error: %v", err)
		}
		for _, tr := range page {
			if seen[tr.ID] {
				t.Fatalf("transfer %s returned more than once across pages", tr.ID)
			}
			seen[tr.ID] = true
			order = append(order, tr.ID)
		}
		if !hasMore {
			break
		}
		last := page[len(page)-1]
		cursor = &pageCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}

	if len(seen) != total {
		t.Fatalf("saw %d distinct transfers across pages, want %d", len(seen), total)
	}
	// Newest first: inserted[total-1] has the latest created_at.
	for i, id := range order {
		want := inserted[total-1-i].ID
		if id != want {
			t.Errorf("order[%d] = %s, want %s (created_at DESC, id DESC)", i, id, want)
		}
	}
}

// TestListTransfersHandler_BatchResolvesCounterpartiesOnce is the
// regression guard for the N+1 fix: however many rows/distinct
// counterparties are on a page, ResolveAccountsByIds must be called
// exactly once per request.
func TestListTransfersHandler_BatchResolvesCounterpartiesOnce(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountA := randomUUIDForTest(t)
	accountB := randomUUIDForTest(t)
	accountC := randomUUIDForTest(t)

	insertPendingTransfer(t, ctx, pool, accountA, accountB, 1000)
	insertPendingTransfer(t, ctx, pool, accountA, accountC, 2000)
	third := insertPendingTransfer(t, ctx, pool, accountA, accountB, 3000)
	if _, err := markTransferCompleted(ctx, pool, third.ID, randomUUIDForTest(t)); err != nil {
		t.Fatalf("markTransferCompleted: %v", err)
	}

	accountNumbers := map[string]string{
		accountB: "NB0000000001",
		accountC: "NB0000000002",
	}

	var resolveCalls int
	client := &fakeAccountsClient{
		getByUserIDFunc: func(ctx context.Context, req *accountsv1.GetAccountByUserIDRequest) (*accountsv1.GetAccountByUserIDResponse, error) {
			return &accountsv1.GetAccountByUserIDResponse{AccountId: accountA, Status: "active"}, nil
		},
		resolveAccountsByIdsFunc: func(ctx context.Context, req *accountsv1.ResolveAccountsByIdsRequest) (*accountsv1.ResolveAccountsByIdsResponse, error) {
			resolveCalls++
			resp := &accountsv1.ResolveAccountsByIdsResponse{}
			for _, id := range req.GetAccountIds() {
				if number, ok := accountNumbers[id]; ok {
					resp.Accounts = append(resp.Accounts, &accountsv1.AccountSummary{AccountId: id, AccountNumber: number})
				}
			}
			return resp, nil
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-User-Id", "irrelevant-userid-fake-handles-lookup")
	rec := httptest.NewRecorder()

	listTransfersHandler(pool, client)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if resolveCalls != 1 {
		t.Errorf("ResolveAccountsByIds called %d times, want exactly 1", resolveCalls)
	}

	var resp listTransfersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Transfers) != 3 {
		t.Fatalf("len(resp.Transfers) = %d, want 3", len(resp.Transfers))
	}
	for _, entry := range resp.Transfers {
		if entry.Direction != "outgoing" {
			t.Errorf("transfer %s direction = %q, want %q", entry.ID, entry.Direction, "outgoing")
		}
		want := accountNumbers[entry.RecipientAccountID]
		if entry.CounterpartyAccountNumber != want {
			t.Errorf("transfer %s counterparty_account_number = %q, want %q", entry.ID, entry.CounterpartyAccountNumber, want)
		}
	}
}

func TestListTransfersHandler_InvalidCursor(t *testing.T) {
	// No live DB needed: an invalid cursor is rejected in
	// parseHistoryQuery, before the handler ever queries pool.
	var pool *pgxpool.Pool

	client := &fakeAccountsClient{
		getByUserIDFunc: func(ctx context.Context, req *accountsv1.GetAccountByUserIDRequest) (*accountsv1.GetAccountByUserIDResponse, error) {
			return &accountsv1.GetAccountByUserIDResponse{AccountId: randomUUIDForTest(t), Status: "active"}, nil
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/?cursor=not-valid-base64!!", nil)
	req.Header.Set("X-User-Id", "irrelevant-userid-fake-handles-lookup")
	rec := httptest.NewRecorder()

	listTransfersHandler(pool, client)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}
