package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// recordedRequest captures exactly what the gateway forwarded to a
// backend, so tests can assert on the path ServeMux/StripPrefix actually
// produced and on byte-for-byte body passthrough (the property Stripe
// webhook signature verification depends on).
type recordedRequest struct {
	method  string
	path    string
	body    []byte
	headers http.Header
}

func newRecordingBackend(t *testing.T) (*httptest.Server, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.body = body
		rec.headers = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// noRedirectClient never follows redirects — a 301 here is exactly the
// failure mode being tested against (see newHandler's doc comment on the
// exact+subtree "/deposits" registration), so it must be visible as a 3xx
// response, not silently followed and hidden.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func signedTestJWT(t *testing.T, secret, userID string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, accessTokenClaims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign test jwt: %v", err)
	}
	return signed
}

func TestNewHandler_DepositsExactPath_NoRedirectAndUnstripped(t *testing.T) {
	backend, rec := newRecordingBackend(t)
	t.Setenv("TRANSFERS_SVC_ADDR", strings.TrimPrefix(backend.URL, "http://"))

	const secret = "test-secret"
	gw := httptest.NewServer(newHandler(context.Background(), secret))
	t.Cleanup(gw.Close)

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/deposits", strings.NewReader(`{"amount":5000}`))
	req.Header.Set("Authorization", "Bearer "+signedTestJWT(t, secret, "user-123"))

	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("POST /deposits: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		t.Fatalf("got redirect status %d — a bare POST /deposits must never redirect (fetch() would silently drop the body on retry)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if rec.method != http.MethodPost {
		t.Errorf("backend saw method %s, want POST", rec.method)
	}
	if rec.path != "/deposits" {
		t.Errorf("backend saw path %q, want %q (unstripped)", rec.path, "/deposits")
	}
	if string(rec.body) != `{"amount":5000}` {
		t.Errorf("backend saw body %q, want unchanged", rec.body)
	}
	if got := rec.headers.Get("X-User-Id"); got != "user-123" {
		t.Errorf("backend saw X-User-Id %q, want %q", got, "user-123")
	}
}

func TestNewHandler_DepositsWithID_NoRedirectAndUnstripped(t *testing.T) {
	backend, rec := newRecordingBackend(t)
	t.Setenv("TRANSFERS_SVC_ADDR", strings.TrimPrefix(backend.URL, "http://"))

	const secret = "test-secret"
	gw := httptest.NewServer(newHandler(context.Background(), secret))
	t.Cleanup(gw.Close)

	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/deposits/abc-123", nil)
	req.Header.Set("Authorization", "Bearer "+signedTestJWT(t, secret, "user-123"))

	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("GET /deposits/abc-123: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if rec.path != "/deposits/abc-123" {
		t.Errorf("backend saw path %q, want %q (unstripped)", rec.path, "/deposits/abc-123")
	}
}

func TestNewHandler_DepositsRequiresJWT(t *testing.T) {
	backend, _ := newRecordingBackend(t)
	t.Setenv("TRANSFERS_SVC_ADDR", strings.TrimPrefix(backend.URL, "http://"))

	gw := httptest.NewServer(newHandler(context.Background(), "test-secret"))
	t.Cleanup(gw.Close)

	resp, err := http.Post(gw.URL+"/deposits", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST /deposits without a token: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d — /deposits must stay JWT-protected", resp.StatusCode, http.StatusUnauthorized)
	}
}

// TestNewHandler_WebhookStripe_PublicAndRawBodyPreserved is the other
// half of task 1: Stripe has no JWT, and its signature check depends on
// receiving the exact bytes it sent — no decode/re-encode anywhere in the
// path, no JSON formatting normalization, no JWT gate.
func TestNewHandler_WebhookStripe_PublicAndRawBodyPreserved(t *testing.T) {
	backend, rec := newRecordingBackend(t)
	t.Setenv("TRANSFERS_SVC_ADDR", strings.TrimPrefix(backend.URL, "http://"))

	gw := httptest.NewServer(newHandler(context.Background(), "test-secret"))
	t.Cleanup(gw.Close)

	// Deliberately odd whitespace/field order: if anything on the path
	// parsed and re-serialized this as JSON, byte-for-byte equality below
	// would fail even though the "meaning" of the payload is unchanged —
	// exactly the failure mode that breaks Stripe's signature check.
	rawBody := `{"type":  "payment_intent.succeeded",   "data":{"object":{}}}`

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/webhooks/stripe", strings.NewReader(rawBody))
	req.Header.Set("Stripe-Signature", "t=0,v1=deadbeef")
	req.Header.Set("X-User-Id", "attacker-supplied-should-be-stripped")
	// No Authorization header at all — this is the point of the test.

	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("POST /webhooks/stripe: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d — /webhooks/stripe must be public (no bearer token required)", resp.StatusCode, http.StatusOK)
	}
	if rec.path != "/webhooks/stripe" {
		t.Errorf("backend saw path %q, want %q", rec.path, "/webhooks/stripe")
	}
	if string(rec.body) != rawBody {
		t.Errorf("backend saw body %q, want byte-for-byte %q", rec.body, rawBody)
	}
	if got := rec.headers.Get("X-User-Id"); got != "" {
		t.Errorf("backend saw X-User-Id %q, want empty — a client-supplied value must always be stripped, even on a public route", got)
	}
	if got := rec.headers.Get("Stripe-Signature"); got != "t=0,v1=deadbeef" {
		t.Errorf("backend saw Stripe-Signature %q, want it forwarded unchanged", got)
	}
}

// TestNewHandler_TransfersPrefixStillStrips is a regression guard: the new
// no-strip routes must not disturb the existing "/transfers" (and
// auth/accounts/fraud/notifications) prefix-stripping behavior.
func TestNewHandler_TransfersPrefixStillStrips(t *testing.T) {
	backend, rec := newRecordingBackend(t)
	t.Setenv("TRANSFERS_SVC_ADDR", strings.TrimPrefix(backend.URL, "http://"))

	const secret = "test-secret"
	gw := httptest.NewServer(newHandler(context.Background(), secret))
	t.Cleanup(gw.Close)

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/transfers/", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+signedTestJWT(t, secret, "user-123"))

	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("POST /transfers/: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if rec.path != "/" {
		t.Errorf("backend saw path %q, want %q (the /transfers prefix must still be stripped)", rec.path, "/")
	}
}
