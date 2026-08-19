package main

import (
	"context"
	crand "crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"neobank/pkg/iban"
	accountsv1 "neobank/proto/gen/go/accounts/v1"
)

const testBankCode = "ZZZZ"

// generateTestIban produces a fresh, checksum-valid IBAN for bankCode with
// random BBAN digits, so tests that need a "some real-shaped IBAN nobody
// has" (not-found / wrong-bank / rate-limit cases) don't collide with each
// other or with anything insertAccountForTest creates.
func generateTestIban(bankCode string) (string, error) {
	sortCode, err := randomDigits(6)
	if err != nil {
		return "", err
	}
	acctNum, err := randomDigits(8)
	if err != nil {
		return "", err
	}
	return iban.Generate(bankCode, sortCode, acctNum)
}

func randomDigits(n int) (string, error) {
	max := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
	v, err := crand.Int(crand.Reader, max)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", n, v.Int64()), nil
}

// formatTestIban renders value the way a human might paste it in (grouped,
// lower case) — exercising ResolveAccountByIban's normalization, not just
// the canonical form.
func formatTestIban(value string) string {
	return strings.ToLower(iban.Format(value))
}

// insertAccountForTest inserts an accounts row with the given iban (and
// freshly generated user_id/account_number, since both are UNIQUE NOT
// NULL), returning its id. Bypasses createAccountForUser entirely — these
// tests are about ResolveAccountByIban's lookup/validation logic, not
// account creation.
func insertAccountForTest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ibanValue string) string {
	t.Helper()
	userID := randomUUIDForTest(t)
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
		if _, err := pool.Exec(ctx, "DELETE FROM accounts WHERE id = $1", id); err != nil {
			t.Logf("cleanup: delete account id=%s: %v", id, err)
		}
	})
	return id
}

// TestResolveAccountByIban_InvalidChecksum is the DoD's explicit
// requirement: a checksum-broken IBAN is rejected without ever reaching
// the database. Uses a nil pool deliberately — if the implementation ever
// regresses to checking the database before the checksum, this test
// panics on the nil pointer dereference instead of quietly passing.
func TestResolveAccountByIban_InvalidChecksum(t *testing.T) {
	s := &accountsServer{pool: nil, bankCode: testBankCode, resolveRateLimit: 10, resolveRateWindow: time.Minute}

	// One digit corrupted relative to a real, checksum-valid reference IBAN.
	_, err := s.ResolveAccountByIban(context.Background(), &accountsv1.ResolveAccountByIbanRequest{
		Iban: "IE29AIBK93115212345679", UserId: randomUUIDForTest(t),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("ResolveAccountByIban error = %v, want codes.InvalidArgument", err)
	}
}

// TestResolveAccountByIban_ShortNonIEDoesNotPanic is the regression guard
// for a real bug found while implementing this method: iban.Validate only
// enforces the fixed 22-character layout for country "IE" — for any other
// country it just checks the generic mod-97 checksum, with no minimum
// length beyond 4. "AT18" is exactly such a string (4 characters,
// checksum-valid, not IE) — naively slicing normalized[4:8] to read out a
// "bank code" after Validate succeeds panics on input this short.
//
// Needs a live pool, unlike the checksum/missing-user_id tests above:
// "AT18" passes the free checksum check, so ResolveAccountByIban reaches
// the rate limiter (its first database access) before the bank-code
// check that this test is actually about — there is no way to reach that
// check without a real pool underneath the rate limiter.
func TestResolveAccountByIban_ShortNonIEDoesNotPanic(t *testing.T) {
	pool := newTestPool(t)
	s := &accountsServer{pool: pool, bankCode: testBankCode, resolveRateLimit: 100, resolveRateWindow: time.Minute}
	userID := randomUUIDForTest(t)
	t.Cleanup(func() { deleteResolveAttempts(t, context.Background(), pool, userID) })

	_, err := s.ResolveAccountByIban(context.Background(), &accountsv1.ResolveAccountByIbanRequest{
		Iban: "AT18", UserId: userID,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ResolveAccountByIban error = %v, want codes.FailedPrecondition", err)
	}
}

// TestResolveAccountByIban_MissingUserID confirms user_id is enforced
// before the rate limiter (and therefore before the database) runs at
// all — an omitted user_id must not silently bypass the rate limit rather
// than being rejected. Nil pool for the same "must not touch the
// database" reason as the checksum test above.
func TestResolveAccountByIban_MissingUserID(t *testing.T) {
	s := &accountsServer{pool: nil, bankCode: testBankCode, resolveRateLimit: 10, resolveRateWindow: time.Minute}

	validIban, err := generateTestIban(testBankCode)
	if err != nil {
		t.Fatalf("generate test iban: %v", err)
	}
	_, err = s.ResolveAccountByIban(context.Background(), &accountsv1.ResolveAccountByIbanRequest{Iban: validIban})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("ResolveAccountByIban error = %v, want codes.InvalidArgument", err)
	}
}

// TestResolveAccountByIban_WrongBank confirms a structurally valid IBAN
// for a bank code that isn't this system's own gets a distinct outcome
// from "not found" — the task's second required distinction ("only
// transfers within this bank are supported" vs. "recipient not found").
func TestResolveAccountByIban_WrongBank(t *testing.T) {
	pool := newTestPool(t)
	s := &accountsServer{pool: pool, bankCode: testBankCode, resolveRateLimit: 100, resolveRateWindow: time.Minute}
	userID := randomUUIDForTest(t)
	t.Cleanup(func() { deleteResolveAttempts(t, context.Background(), pool, userID) })

	foreignIban, err := generateTestIban("YYYY")
	if err != nil {
		t.Fatalf("generate test iban: %v", err)
	}

	_, err = s.ResolveAccountByIban(context.Background(), &accountsv1.ResolveAccountByIbanRequest{Iban: foreignIban, UserId: userID})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ResolveAccountByIban error = %v, want codes.FailedPrecondition", err)
	}
}

// TestResolveAccountByIban_NotFound confirms this bank's own IBAN with no
// matching account gets the third, distinct outcome.
func TestResolveAccountByIban_NotFound(t *testing.T) {
	pool := newTestPool(t)
	s := &accountsServer{pool: pool, bankCode: testBankCode, resolveRateLimit: 100, resolveRateWindow: time.Minute}
	userID := randomUUIDForTest(t)
	t.Cleanup(func() { deleteResolveAttempts(t, context.Background(), pool, userID) })

	unusedIban, err := generateTestIban(testBankCode)
	if err != nil {
		t.Fatalf("generate test iban: %v", err)
	}

	_, err = s.ResolveAccountByIban(context.Background(), &accountsv1.ResolveAccountByIbanRequest{Iban: unusedIban, UserId: userID})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("ResolveAccountByIban error = %v, want codes.NotFound", err)
	}
}

// TestResolveAccountByIban_Found is the success path: a real account's
// IBAN resolves to its account_id and status, and normalization (spaces,
// lower case) is applied before lookup — a user pasting a formatted IBAN
// must still resolve.
func TestResolveAccountByIban_Found(t *testing.T) {
	pool := newTestPool(t)
	s := &accountsServer{pool: pool, bankCode: testBankCode, resolveRateLimit: 100, resolveRateWindow: time.Minute}
	userID := randomUUIDForTest(t)
	t.Cleanup(func() { deleteResolveAttempts(t, context.Background(), pool, userID) })

	targetIban, err := generateTestIban(testBankCode)
	if err != nil {
		t.Fatalf("generate test iban: %v", err)
	}
	accountID := insertAccountForTest(t, context.Background(), pool, targetIban)

	resp, err := s.ResolveAccountByIban(context.Background(), &accountsv1.ResolveAccountByIbanRequest{
		Iban: formatTestIban(targetIban), UserId: userID,
	})
	if err != nil {
		t.Fatalf("ResolveAccountByIban: unexpected error: %v", err)
	}
	if resp.GetAccountId() != accountID {
		t.Errorf("AccountId = %q, want %q", resp.GetAccountId(), accountID)
	}
	if resp.GetStatus() != "active" {
		t.Errorf("Status = %q, want %q", resp.GetStatus(), "active")
	}
}

// TestResolveAccountByIban_RateLimited confirms the fourth outcome: too
// many attempts from the same user, regardless of what IBAN they're
// probing.
func TestResolveAccountByIban_RateLimited(t *testing.T) {
	pool := newTestPool(t)
	s := &accountsServer{pool: pool, bankCode: testBankCode, resolveRateLimit: 1, resolveRateWindow: time.Minute}
	userID := randomUUIDForTest(t)
	t.Cleanup(func() { deleteResolveAttempts(t, context.Background(), pool, userID) })

	iban1, err := generateTestIban(testBankCode)
	if err != nil {
		t.Fatalf("generate test iban: %v", err)
	}
	iban2, err := generateTestIban(testBankCode)
	if err != nil {
		t.Fatalf("generate test iban: %v", err)
	}

	// First attempt consumes the (limit=1) budget — expected to reach the
	// database and come back NotFound, not blocked.
	_, err = s.ResolveAccountByIban(context.Background(), &accountsv1.ResolveAccountByIbanRequest{Iban: iban1, UserId: userID})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("first ResolveAccountByIban error = %v, want codes.NotFound", err)
	}

	// Second attempt, a DIFFERENT IBAN entirely — proves the limit is
	// keyed on the user, not on which IBAN is being probed.
	_, err = s.ResolveAccountByIban(context.Background(), &accountsv1.ResolveAccountByIbanRequest{Iban: iban2, UserId: userID})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("second ResolveAccountByIban error = %v, want codes.ResourceExhausted", err)
	}
}
