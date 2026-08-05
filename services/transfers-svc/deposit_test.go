package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stripe/stripe-go/v86"

	accountsv1 "neobank/proto/gen/go/accounts/v1"
)

// fakePaymentIntentCreator implements stripePaymentIntentCreator without a
// live Stripe account — same fake-by-function-field shape as
// fakeAccountsClient/fakeLedgerClient, letting tests assert exactly what
// createDeposit sent Stripe without ever making a real API call.
type fakePaymentIntentCreator struct {
	createFunc func(ctx context.Context, params *stripe.PaymentIntentCreateParams) (*stripe.PaymentIntent, error)
}

func (f *fakePaymentIntentCreator) Create(ctx context.Context, params *stripe.PaymentIntentCreateParams) (*stripe.PaymentIntent, error) {
	return f.createFunc(ctx, params)
}

func TestCreateDeposit_Success(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountID := randomUUIDForTest(t)
	accountsClient := &fakeAccountsClient{
		getByIDFunc: func(ctx context.Context, req *accountsv1.GetAccountByIDRequest) (*accountsv1.GetAccountByIDResponse, error) {
			if req.GetAccountId() != accountID {
				t.Errorf("GetAccountByID account_id = %s, want %s", req.GetAccountId(), accountID)
			}
			return &accountsv1.GetAccountByIDResponse{AccountId: accountID, Status: accountStatusActive}, nil
		},
	}

	wantIntentID := "pi_" + randomUUIDForTest(t)
	wantClientSecret := "secret_" + randomUUIDForTest(t)
	var gotDepositIDAtCallTime string
	paymentIntents := &fakePaymentIntentCreator{
		createFunc: func(ctx context.Context, params *stripe.PaymentIntentCreateParams) (*stripe.PaymentIntent, error) {
			if params.IdempotencyKey == nil || *params.IdempotencyKey == "" {
				t.Error("PaymentIntentCreateParams: IdempotencyKey is empty, want the deposit id")
			}
			gotDepositIDAtCallTime = *params.IdempotencyKey
			if params.Metadata["deposit_id"] != gotDepositIDAtCallTime {
				t.Errorf("metadata[deposit_id] = %q, want %q (must match IdempotencyKey)", params.Metadata["deposit_id"], gotDepositIDAtCallTime)
			}
			if params.Metadata["account_id"] != accountID {
				t.Errorf("metadata[account_id] = %q, want %q", params.Metadata["account_id"], accountID)
			}
			if params.Amount == nil || *params.Amount != 5000 {
				t.Errorf("Amount = %v, want 5000", params.Amount)
			}
			if params.Currency == nil || *params.Currency != "eur" {
				t.Errorf("Currency = %v, want %q", params.Currency, "eur")
			}
			return &stripe.PaymentIntent{ID: wantIntentID, ClientSecret: wantClientSecret}, nil
		},
	}

	deposit, clientSecret, outcome, err := createDeposit(ctx, pool, accountsClient, paymentIntents, accountID, 5000)
	if err != nil {
		t.Fatalf("createDeposit: unexpected error: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM deposits WHERE id = $1", deposit.ID); err != nil {
			t.Logf("cleanup: delete deposit id=%s: %v", deposit.ID, err)
		}
	})

	if outcome != createDepositOK {
		t.Fatalf("createDeposit outcome = %v, want createDepositOK", outcome)
	}
	if deposit.ID == "" {
		t.Error("deposit.ID is empty")
	}
	if deposit.ID != gotDepositIDAtCallTime {
		t.Errorf("deposit.ID = %s, want %s (the id Stripe was actually called with)", deposit.ID, gotDepositIDAtCallTime)
	}
	if deposit.AccountID != accountID {
		t.Errorf("deposit.AccountID = %s, want %s", deposit.AccountID, accountID)
	}
	if deposit.Amount != 5000 {
		t.Errorf("deposit.Amount = %d, want 5000", deposit.Amount)
	}
	if deposit.Status != "pending" {
		t.Errorf("deposit.Status = %q, want %q — Stripe confirming the charge (succeeded) and us crediting the ledger (credited) are both still in the future", deposit.Status, "pending")
	}
	if deposit.StripePaymentIntentID == nil || *deposit.StripePaymentIntentID != wantIntentID {
		t.Errorf("deposit.StripePaymentIntentID = %v, want %s", deposit.StripePaymentIntentID, wantIntentID)
	}
	if clientSecret != wantClientSecret {
		t.Errorf("clientSecret = %s, want %s", clientSecret, wantClientSecret)
	}
}

func TestCreateDeposit_InvalidAmount(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	accountsClient := &fakeAccountsClient{
		getByIDFunc: func(ctx context.Context, req *accountsv1.GetAccountByIDRequest) (*accountsv1.GetAccountByIDResponse, error) {
			t.Fatal("GetAccountByID should not be called for an amount that fails validation before any lookup")
			return nil, nil
		},
	}
	paymentIntents := &fakePaymentIntentCreator{
		createFunc: func(ctx context.Context, params *stripe.PaymentIntentCreateParams) (*stripe.PaymentIntent, error) {
			t.Fatal("Stripe should never be called for an invalid amount")
			return nil, nil
		},
	}

	for _, amount := range []int64{0, -100, depositMinAmount - 1, depositMaxAmount + 1} {
		deposit, clientSecret, outcome, err := createDeposit(ctx, pool, accountsClient, paymentIntents, randomUUIDForTest(t), amount)
		if err != nil {
			t.Fatalf("createDeposit(amount=%d): unexpected error: %v", amount, err)
		}
		if outcome != createDepositInvalidAmount {
			t.Errorf("createDeposit(amount=%d) outcome = %v, want createDepositInvalidAmount", amount, outcome)
		}
		if deposit.ID != "" || clientSecret != "" {
			t.Errorf("createDeposit(amount=%d): got non-empty deposit/clientSecret on a rejected amount", amount)
		}
	}
}

func TestCreateDeposit_AccountNotActive(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	accountID := randomUUIDForTest(t)

	accountsClient := &fakeAccountsClient{
		getByIDFunc: func(ctx context.Context, req *accountsv1.GetAccountByIDRequest) (*accountsv1.GetAccountByIDResponse, error) {
			return &accountsv1.GetAccountByIDResponse{AccountId: accountID, Status: "frozen"}, nil
		},
	}
	paymentIntents := &fakePaymentIntentCreator{
		createFunc: func(ctx context.Context, params *stripe.PaymentIntentCreateParams) (*stripe.PaymentIntent, error) {
			t.Fatal("Stripe should never be called when the account isn't active")
			return nil, nil
		},
	}

	deposit, _, outcome, err := createDeposit(ctx, pool, accountsClient, paymentIntents, accountID, 5000)
	if err != nil {
		t.Fatalf("createDeposit: unexpected error: %v", err)
	}
	if outcome != createDepositAccountNotActive {
		t.Fatalf("createDeposit outcome = %v, want createDepositAccountNotActive", outcome)
	}
	if deposit.ID != "" {
		t.Error("createDeposit: got a non-empty deposit for a not-active account — no row should have been inserted")
	}
}

// TestCreateDeposit_StripeErrorLeavesRowPending proves the safety property
// documented on createDeposit: if the Stripe API call fails after the
// pending deposits row has already been inserted, that row is left exactly
// as it is — status 'pending', stripe_payment_intent_id still NULL — rather
// than being deleted or guessed at. No money has moved either way.
func TestCreateDeposit_StripeErrorLeavesRowPending(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	accountID := randomUUIDForTest(t)

	accountsClient := &fakeAccountsClient{
		getByIDFunc: func(ctx context.Context, req *accountsv1.GetAccountByIDRequest) (*accountsv1.GetAccountByIDResponse, error) {
			return &accountsv1.GetAccountByIDResponse{AccountId: accountID, Status: accountStatusActive}, nil
		},
	}
	paymentIntents := &fakePaymentIntentCreator{
		createFunc: func(ctx context.Context, params *stripe.PaymentIntentCreateParams) (*stripe.PaymentIntent, error) {
			return nil, errors.New("simulated stripe network failure")
		},
	}

	_, _, _, err := createDeposit(ctx, pool, accountsClient, paymentIntents, accountID, 5000)
	if err == nil {
		t.Fatal("createDeposit: expected an error when the Stripe call fails, got nil")
	}

	var status string
	var stripePaymentIntentID *string
	var depositID string
	scanErr := pool.QueryRow(ctx,
		"SELECT id, status, stripe_payment_intent_id FROM deposits WHERE account_id = $1",
		accountID,
	).Scan(&depositID, &status, &stripePaymentIntentID)
	if scanErr != nil {
		t.Fatalf("query the deposit left behind by the failed attempt: %v", scanErr)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM deposits WHERE id = $1", depositID); err != nil {
			t.Logf("cleanup: delete deposit id=%s: %v", depositID, err)
		}
	})

	if status != "pending" {
		t.Errorf("leftover deposit status = %q, want %q", status, "pending")
	}
	if stripePaymentIntentID != nil {
		t.Errorf("leftover deposit stripe_payment_intent_id = %v, want nil", stripePaymentIntentID)
	}
}

// newGetDepositRequest builds a GET /deposits/{id} request with the path
// value set directly (Request.SetPathValue, Go 1.22+) rather than routed
// through a live mux — the same shortcut this codebase has no prior
// example of yet, since accounts-svc's GET /{id} has no handler-level test
// of its own; this is the natural way to unit-test a wildcard-pattern
// handler without standing up the whole ServeMux.
func newGetDepositRequest(t *testing.T, id, userID string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/deposits/"+id, nil)
	req.SetPathValue("id", id)
	req.Header.Set("X-User-Id", userID)
	return req
}

func TestGetDepositHandler_Success(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	accountID := randomUUIDForTest(t)

	depositID := insertDepositForReconcile(t, ctx, pool, accountID, 5000, "credited", strPtrForTest("pi_test123"))

	client := &fakeAccountsClient{
		getByUserIDFunc: func(ctx context.Context, req *accountsv1.GetAccountByUserIDRequest) (*accountsv1.GetAccountByUserIDResponse, error) {
			return &accountsv1.GetAccountByUserIDResponse{AccountId: accountID, Status: "active"}, nil
		},
	}

	rec := httptest.NewRecorder()
	getDepositHandler(pool, client)(rec, newGetDepositRequest(t, depositID, "irrelevant-userid-fake-handles-lookup"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var got Deposit
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ID != depositID || got.Status != "credited" || got.Amount != 5000 {
		t.Errorf("got deposit %+v, want id=%s status=credited amount=5000", got, depositID)
	}
}

func TestGetDepositHandler_NotFound(t *testing.T) {
	pool := newTestPool(t)
	accountID := randomUUIDForTest(t)

	client := &fakeAccountsClient{
		getByUserIDFunc: func(ctx context.Context, req *accountsv1.GetAccountByUserIDRequest) (*accountsv1.GetAccountByUserIDResponse, error) {
			return &accountsv1.GetAccountByUserIDResponse{AccountId: accountID, Status: "active"}, nil
		},
	}

	rec := httptest.NewRecorder()
	getDepositHandler(pool, client)(rec, newGetDepositRequest(t, randomUUIDForTest(t), "irrelevant"))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

// TestGetDepositHandler_WrongAccount proves a caller can never read
// another account's deposit by guessing/enumerating ids — the response is
// indistinguishable (404, same as a genuinely missing id) from
// TestGetDepositHandler_NotFound, on purpose.
func TestGetDepositHandler_WrongAccount(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	ownerAccountID := randomUUIDForTest(t)
	callerAccountID := randomUUIDForTest(t)

	depositID := insertDepositForReconcile(t, ctx, pool, ownerAccountID, 5000, "pending", nil)

	client := &fakeAccountsClient{
		getByUserIDFunc: func(ctx context.Context, req *accountsv1.GetAccountByUserIDRequest) (*accountsv1.GetAccountByUserIDResponse, error) {
			return &accountsv1.GetAccountByUserIDResponse{AccountId: callerAccountID, Status: "active"}, nil
		},
	}

	rec := httptest.NewRecorder()
	getDepositHandler(pool, client)(rec, newGetDepositRequest(t, depositID, "irrelevant"))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d (deposit belongs to a different account); body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

// TestGetDepositHandler_MalformedID proves a non-UUID path segment (never
// producible by this frontend, but nothing stops an arbitrary HTTP client
// from sending one) resolves to the same 404 as a genuinely missing
// deposit, not a 500 — see getDepositByID's invalidTextRepresentation
// handling.
func TestGetDepositHandler_MalformedID(t *testing.T) {
	pool := newTestPool(t)
	accountID := randomUUIDForTest(t)

	client := &fakeAccountsClient{
		getByUserIDFunc: func(ctx context.Context, req *accountsv1.GetAccountByUserIDRequest) (*accountsv1.GetAccountByUserIDResponse, error) {
			return &accountsv1.GetAccountByUserIDResponse{AccountId: accountID, Status: "active"}, nil
		},
	}

	rec := httptest.NewRecorder()
	getDepositHandler(pool, client)(rec, newGetDepositRequest(t, "not-a-uuid", "irrelevant"))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func strPtrForTest(s string) *string { return &s }
