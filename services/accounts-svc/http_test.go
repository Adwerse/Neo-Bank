package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

// fakeLedgerClientForHTTP implements ledgerv1.LedgerServiceClient without a
// live ledger-svc, same pattern as transfers-svc's fakeLedgerClient
// (transfer_test.go) — embed the real interface as nil so only
// GetBalanceHistory, the one method balanceHistoryHandler calls, needs
// overriding.
type fakeLedgerClientForHTTP struct {
	ledgerv1.LedgerServiceClient
	getBalanceHistoryFunc func(ctx context.Context, req *ledgerv1.GetBalanceHistoryRequest) (*ledgerv1.GetBalanceHistoryResponse, error)
}

func (f *fakeLedgerClientForHTTP) GetBalanceHistory(ctx context.Context, req *ledgerv1.GetBalanceHistoryRequest, opts ...grpc.CallOption) (*ledgerv1.GetBalanceHistoryResponse, error) {
	return f.getBalanceHistoryFunc(ctx, req)
}

// insertAccountForTestUser inserts an accounts row for a caller-chosen
// userID — unlike grpc_server_test.go's insertAccountForTest, which mints
// its own random user_id, balanceHistoryHandler tests need a userID they
// can put on the X-User-Id header to reach this specific account.
func insertAccountForTestUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID, ibanValue string) string {
	t.Helper()
	accountNumber, err := generateAccountNumber()
	if err != nil {
		t.Fatalf("generate account number: %v", err)
	}
	var id string
	err = pool.QueryRow(ctx,
		"INSERT INTO accounts (user_id, account_number, iban) VALUES ($1, $2, $3) RETURNING id",
		userID, accountNumber, ibanValue,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert test account: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM accounts WHERE id = $1", id); err != nil {
			t.Logf("cleanup: delete account id=%s: %v", id, err)
		}
	})
	return id
}

func mustParseDate(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		t.Fatalf("parse date %q: %v", value, err)
	}
	return parsed
}

func TestBalanceHistoryHandler_InvalidRange(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/me/balance-history?range=year", nil)
	req.Header.Set("X-User-Id", randomUUIDForTest(t))

	balanceHistoryHandler(nil, &fakeLedgerClientForHTTP{})(rec, req)

	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestBalanceHistoryHandler_AccountNotFound(t *testing.T) {
	pool := newTestPool(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/me/balance-history?range=week", nil)
	req.Header.Set("X-User-Id", randomUUIDForTest(t)) // no accounts row for this user

	balanceHistoryHandler(pool, &fakeLedgerClientForHTTP{})(rec, req)

	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestBalanceHistoryHandler_LedgerUnavailable(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	userID := randomUUIDForTest(t)
	ibanValue, err := generateTestIban(testBankCode)
	if err != nil {
		t.Fatalf("generate test iban: %v", err)
	}
	insertAccountForTestUser(t, ctx, pool, userID, ibanValue)

	ledgerClient := &fakeLedgerClientForHTTP{
		getBalanceHistoryFunc: func(ctx context.Context, req *ledgerv1.GetBalanceHistoryRequest) (*ledgerv1.GetBalanceHistoryResponse, error) {
			return nil, status.Error(codes.Unavailable, "simulated ledger-svc outage")
		},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/me/balance-history?range=week", nil)
	req.Header.Set("X-User-Id", userID)

	balanceHistoryHandler(pool, ledgerClient)(rec, req)

	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 (never a fake 200 when ledger-svc is unreachable)", rec.Code)
	}
}

// TestBalanceHistoryHandler_HappyPath covers all three range values,
// confirming the handler translates each into the from cutoff
// GetBalanceHistory actually receives (or nil, for "all") and reshapes the
// response into the day-string JSON the frontend expects.
func TestBalanceHistoryHandler_HappyPath(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	userID := randomUUIDForTest(t)
	ibanValue, err := generateTestIban(testBankCode)
	if err != nil {
		t.Fatalf("generate test iban: %v", err)
	}
	accountID := insertAccountForTestUser(t, ctx, pool, userID, ibanValue)

	point := &ledgerv1.BalancePoint{
		Date:    timestamppb.New(mustParseDate(t, "2026-08-20")),
		Balance: 4200,
	}

	for _, rangeParam := range []string{"week", "month", "all"} {
		t.Run(rangeParam, func(t *testing.T) {
			var gotReq *ledgerv1.GetBalanceHistoryRequest
			ledgerClient := &fakeLedgerClientForHTTP{
				getBalanceHistoryFunc: func(ctx context.Context, req *ledgerv1.GetBalanceHistoryRequest) (*ledgerv1.GetBalanceHistoryResponse, error) {
					gotReq = req
					return &ledgerv1.GetBalanceHistoryResponse{Points: []*ledgerv1.BalancePoint{point}}, nil
				},
			}

			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/me/balance-history?range="+rangeParam, nil)
			req.Header.Set("X-User-Id", userID)

			balanceHistoryHandler(pool, ledgerClient)(rec, req)

			if rec.Code != 200 {
				t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}
			if gotReq.GetAccountId() != accountID {
				t.Errorf("GetBalanceHistory request AccountId = %q, want %q", gotReq.GetAccountId(), accountID)
			}
			if rangeParam == "all" {
				if gotReq.GetFrom() != nil {
					t.Errorf("range=all: GetBalanceHistory request From = %v, want nil", gotReq.GetFrom())
				}
			} else if gotReq.GetFrom() == nil {
				t.Errorf("range=%s: GetBalanceHistory request From = nil, want a cutoff timestamp", rangeParam)
			}

			var body balanceHistoryResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal response body: %v", err)
			}
			if len(body.Points) != 1 {
				t.Fatalf("response Points = %d, want 1", len(body.Points))
			}
			if body.Points[0].Date != "2026-08-20" {
				t.Errorf("Points[0].Date = %q, want %q", body.Points[0].Date, "2026-08-20")
			}
			if body.Points[0].Balance != 4200 {
				t.Errorf("Points[0].Balance = %d, want 4200", body.Points[0].Balance)
			}
		})
	}
}
