package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAccountCache_SetThenGet_ReturnsWithinTTL(t *testing.T) {
	c := newAccountCache(time.Hour, "unused:0")
	c.set("acct-1", "user-1")

	got, ok := c.get("acct-1")
	if !ok {
		t.Fatal("expected a cache hit right after set")
	}
	if got != "user-1" {
		t.Errorf("got %q, want %q", got, "user-1")
	}
}

func TestAccountCache_ExpiresAfterTTL(t *testing.T) {
	c := newAccountCache(50*time.Millisecond, "unused:0")
	c.set("acct-1", "user-1")

	time.Sleep(150 * time.Millisecond)

	if _, ok := c.get("acct-1"); ok {
		t.Error("expected the entry to have expired")
	}
}

func TestAccountCache_Get_MissForUnknownAccount(t *testing.T) {
	c := newAccountCache(time.Hour, "unused:0")
	if _, ok := c.get("never-set"); ok {
		t.Error("expected a miss for an account that was never set")
	}
}

// TestAccountCache_ResolveFallsBackToHTTPOnMiss proves both halves of the
// fallback: a cold cache reaches accounts-svc and gets the answer, and a
// warm one does NOT — the second resolve() for the same account_id must
// not cost a second HTTP call.
func TestAccountCache_ResolveFallsBackToHTTPOnMiss(t *testing.T) {
	var requests int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"user_id": "user-42"})
	}))
	t.Cleanup(backend.Close)

	c := newAccountCache(time.Hour, backend.Listener.Addr().String())

	got, ok := c.resolve(context.Background(), "acct-1")
	if !ok {
		t.Fatal("expected resolve to succeed via the HTTP fallback")
	}
	if got != "user-42" {
		t.Errorf("got %q, want %q", got, "user-42")
	}
	if requests != 1 {
		t.Fatalf("accounts-svc received %d requests after the first resolve, want 1", requests)
	}

	got2, ok2 := c.resolve(context.Background(), "acct-1")
	if !ok2 || got2 != "user-42" {
		t.Fatalf("second resolve = (%q, %v), want (%q, true)", got2, ok2, "user-42")
	}
	if requests != 1 {
		t.Errorf("accounts-svc received %d requests after a cached second resolve, want still 1 — the cache should have short-circuited the HTTP call", requests)
	}
}

func TestAccountCache_Resolve_NotFoundReturnsNotOK(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(backend.Close)

	c := newAccountCache(time.Hour, backend.Listener.Addr().String())

	_, ok := c.resolve(context.Background(), "acct-missing")
	if ok {
		t.Error("expected resolve to fail for a 404 response")
	}
}

func TestAccountCache_Resolve_MalformedJSONReturnsNotOK(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("not json"))
	}))
	t.Cleanup(backend.Close)

	c := newAccountCache(time.Hour, backend.Listener.Addr().String())

	_, ok := c.resolve(context.Background(), "acct-1")
	if ok {
		t.Error("expected resolve to fail for a malformed response body")
	}
}
