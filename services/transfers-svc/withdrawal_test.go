package main

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountsv1 "neobank/proto/gen/go/accounts/v1"
	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

func TestCreateWithdrawal_Success(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUIDForTest(t)
	accountsClient := &fakeAccountsClient{
		getByIDFunc: func(ctx context.Context, req *accountsv1.GetAccountByIDRequest) (*accountsv1.GetAccountByIDResponse, error) {
			return &accountsv1.GetAccountByIDResponse{AccountId: accountID, Status: accountStatusActive}, nil
		},
	}

	wantTxnID := randomUUIDForTest(t)
	ledgerClient := &fakeLedgerClient{
		executeTransferFunc: func(ctx context.Context, req *ledgerv1.ExecuteTransferRequest) (*ledgerv1.ExecuteTransferResponse, error) {
			if req.GetFromAccountId() != accountID {
				t.Errorf("ExecuteTransfer FromAccountId = %s, want %s", req.GetFromAccountId(), accountID)
			}
			if req.GetToAccountId() != genesisAccountID {
				t.Errorf("ExecuteTransfer ToAccountId = %s, want genesis %s", req.GetToAccountId(), genesisAccountID)
			}
			if req.GetAmount() != 5000 {
				t.Errorf("ExecuteTransfer Amount = %d, want 5000", req.GetAmount())
			}
			return &ledgerv1.ExecuteTransferResponse{TransactionId: wantTxnID}, nil
		},
	}

	w, outcome, err := createWithdrawal(ctx, pool, accountsClient, ledgerClient, accountID, 5000)
	if err != nil {
		t.Fatalf("createWithdrawal: unexpected error: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM withdrawals WHERE id = $1", w.ID); err != nil {
			t.Logf("cleanup: delete withdrawal id=%s: %v", w.ID, err)
		}
	})

	if outcome != createWithdrawalPayoutSimulated {
		t.Fatalf("outcome = %v, want createWithdrawalPayoutSimulated", outcome)
	}
	if w.Status != "payout_simulated" {
		t.Errorf("w.Status = %q, want %q", w.Status, "payout_simulated")
	}
	if w.LedgerTransactionID == nil || *w.LedgerTransactionID != wantTxnID {
		t.Errorf("w.LedgerTransactionID = %v, want %s", w.LedgerTransactionID, wantTxnID)
	}
	if w.FailureReason != nil {
		t.Errorf("w.FailureReason = %v, want nil", w.FailureReason)
	}
}

func TestCreateWithdrawal_InsufficientFunds(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUIDForTest(t)
	accountsClient := &fakeAccountsClient{
		getByIDFunc: func(ctx context.Context, req *accountsv1.GetAccountByIDRequest) (*accountsv1.GetAccountByIDResponse, error) {
			return &accountsv1.GetAccountByIDResponse{AccountId: accountID, Status: accountStatusActive}, nil
		},
	}
	ledgerClient := &fakeLedgerClient{
		executeTransferFunc: func(ctx context.Context, req *ledgerv1.ExecuteTransferRequest) (*ledgerv1.ExecuteTransferResponse, error) {
			return nil, status.Error(codes.FailedPrecondition, "insufficient funds")
		},
	}

	w, outcome, err := createWithdrawal(ctx, pool, accountsClient, ledgerClient, accountID, 5000)
	if err != nil {
		t.Fatalf("createWithdrawal: unexpected error: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM withdrawals WHERE id = $1", w.ID); err != nil {
			t.Logf("cleanup: delete withdrawal id=%s: %v", w.ID, err)
		}
	})

	if outcome != createWithdrawalInsufficientFunds {
		t.Fatalf("outcome = %v, want createWithdrawalInsufficientFunds", outcome)
	}
	if w.Status != "failed" {
		t.Errorf("w.Status = %q, want %q", w.Status, "failed")
	}
	if w.FailureReason == nil || *w.FailureReason != "insufficient_funds" {
		t.Errorf("w.FailureReason = %v, want %q", w.FailureReason, "insufficient_funds")
	}
	if w.LedgerTransactionID != nil {
		t.Errorf("w.LedgerTransactionID = %v, want nil (no money moved)", w.LedgerTransactionID)
	}
}

func TestCreateWithdrawal_InvalidAmount(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountsClient := &fakeAccountsClient{
		getByIDFunc: func(ctx context.Context, req *accountsv1.GetAccountByIDRequest) (*accountsv1.GetAccountByIDResponse, error) {
			t.Fatal("GetAccountByID should not be called for an amount that fails validation before any lookup")
			return nil, nil
		},
	}
	ledgerClient := &fakeLedgerClient{
		executeTransferFunc: func(ctx context.Context, req *ledgerv1.ExecuteTransferRequest) (*ledgerv1.ExecuteTransferResponse, error) {
			t.Fatal("ledger-svc should never be called for an invalid amount")
			return nil, nil
		},
	}

	for _, amount := range []int64{0, -100, withdrawMinAmount - 1, withdrawMaxAmount + 1} {
		w, outcome, err := createWithdrawal(ctx, pool, accountsClient, ledgerClient, randomUUIDForTest(t), amount)
		if err != nil {
			t.Fatalf("createWithdrawal(amount=%d): unexpected error: %v", amount, err)
		}
		if outcome != createWithdrawalInvalidAmount {
			t.Errorf("createWithdrawal(amount=%d) outcome = %v, want createWithdrawalInvalidAmount", amount, outcome)
		}
		if w.ID != "" {
			t.Errorf("createWithdrawal(amount=%d): got a non-empty withdrawal for a rejected amount", amount)
		}
	}
}

func TestCreateWithdrawal_AccountNotActive(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	accountID := randomUUIDForTest(t)

	accountsClient := &fakeAccountsClient{
		getByIDFunc: func(ctx context.Context, req *accountsv1.GetAccountByIDRequest) (*accountsv1.GetAccountByIDResponse, error) {
			return &accountsv1.GetAccountByIDResponse{AccountId: accountID, Status: "frozen"}, nil
		},
	}
	ledgerClient := &fakeLedgerClient{
		executeTransferFunc: func(ctx context.Context, req *ledgerv1.ExecuteTransferRequest) (*ledgerv1.ExecuteTransferResponse, error) {
			t.Fatal("ledger-svc should never be called when the account isn't active")
			return nil, nil
		},
	}

	w, outcome, err := createWithdrawal(ctx, pool, accountsClient, ledgerClient, accountID, 5000)
	if err != nil {
		t.Fatalf("createWithdrawal: unexpected error: %v", err)
	}
	if outcome != createWithdrawalAccountNotActive {
		t.Fatalf("outcome = %v, want createWithdrawalAccountNotActive", outcome)
	}
	if w.ID != "" {
		t.Error("createWithdrawal: got a non-empty withdrawal for a not-active account — no row should have been inserted")
	}
}
