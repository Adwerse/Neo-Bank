package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestInit_SucceedsWithoutACollector is the regression test for a bug that
// disabled tracing in all seven services at once, silently.
//
// Init merged a resource built from semconv v1.26.0 into
// resource.Default(), which the SDK builds from v1.43.0. resource.Merge
// refuses to merge resources whose schema URLs differ and returns an
// error, so every service logged one line — "tracing disabled:
// conflicting Schema URL" — and carried on perfectly happily, producing
// no spans at all. Jaeger listed exactly one service: itself.
//
// Nothing about that failure was visible from the outside: the stack was
// healthy, requests succeeded, and the only symptom was an absence. This
// test asserts the thing that actually broke — that Init returns without
// error — and it needs no collector to do it, because the OTLP exporter
// connects lazily.
func TestInit_SucceedsWithoutACollector(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "127.0.0.1:4317")

	shutdown, err := Init(context.Background(), "test-svc")
	if err != nil {
		t.Fatalf("Init returned %v — tracing would be silently disabled in every service", err)
	}
	if shutdown == nil {
		t.Fatal("Init returned a nil shutdown func")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown returned %v", err)
	}
}

// TestInit_RespectsSDKDisabled covers the documented escape hatch for
// running the stack without Jaeger.
func TestInit_RespectsSDKDisabled(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")

	shutdown, err := Init(context.Background(), "test-svc")
	if err != nil {
		t.Fatalf("Init returned %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown returned %v", err)
	}
}

// TestSpanName pins the cardinality control. Span names become Jaeger's
// operation list, so a raw UUID in a path would add a new permanent entry
// per request and make that list useless.
func TestSpanName(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   string
	}{
		{"POST", "/transfers/", "POST /transfers/"},
		{"GET", "/deposits/9a1c2d3e-4f50-4a6b-8c7d-0e1f2a3b4c5d", "GET /deposits/{id}"},
		{"GET", "/accounts/me", "GET /accounts/me"},
		{"GET", "/transfers/42", "GET /transfers/{id}"},
		{"POST", "/webhooks/stripe", "POST /webhooks/stripe"},
		// A UUID in a non-terminal segment must collapse too.
		{"GET", "/a/9a1c2d3e-4f50-4a6b-8c7d-0e1f2a3b4c5d/b", "GET /a/{id}/b"},
	}

	for _, tt := range tests {
		t.Run(tt.method+tt.path, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, tt.path, nil)
			if got := SpanName(r); got != tt.want {
				t.Errorf("SpanName = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestIsNoise guards the two exclusions, both of which matter for
// different reasons — see isNoise's doc comment. The /ws case is the one
// with teeth: a traced WebSocket produces a span that stays open for the
// life of the connection.
func TestIsNoise(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/healthz", true},
		{"/accounts/healthz", true},
		{"/ws", true},
		{"/transfers/", false},
		{"/accounts/me", false},
		{"/", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tt.path, nil)
			if got := isNoise(r); got != tt.want {
				t.Errorf("isNoise(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// TestHandler_PassesThroughFilteredRequests checks that a filtered request
// still reaches the handler — the filter suppresses the span, not the
// request. A regression here would take /healthz and /ws offline entirely.
func TestHandler_PassesThroughFilteredRequests(t *testing.T) {
	called := false
	h := Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}), "test-svc")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if !called {
		t.Error("filtered request never reached the wrapped handler")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}
