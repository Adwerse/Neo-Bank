package tracing

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// withRecorder installs a real SDK provider that records spans in memory,
// so these tests assert on what would actually be exported rather than on
// the no-op provider (which silently discards everything and would make
// every assertion below pass vacuously).
func withRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(recorder),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	prevProvider := otel.GetTracerProvider()
	prevPropagator := otel.GetTextMapPropagator()
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	t.Cleanup(func() {
		otel.SetTracerProvider(prevProvider)
		otel.SetTextMapPropagator(prevPropagator)
	})
	return recorder
}

// startSpan begins a span and returns its context, standing in for "an
// HTTP request is being handled".
func startSpan(t *testing.T, name string) (context.Context, trace.Span) {
	t.Helper()
	return otel.Tracer("test").Start(context.Background(), name)
}

// TestSerializeContext_RoundTripsThroughStorage is the core of the
// outbox-gap mechanism: a context written to a database column must come
// back out identifying the same trace. If this breaks, every async trace
// silently detaches and each background span becomes its own root — which
// looks like working tracing until someone tries to follow a transfer.
func TestSerializeContext_RoundTripsThroughStorage(t *testing.T) {
	withRecorder(t)

	ctx, span := startSpan(t, "POST /transfers/")
	defer span.End()
	original := trace.SpanContextFromContext(ctx)

	serialized := SerializeContext(ctx)
	if serialized == "" {
		t.Fatal("SerializeContext returned empty for a context with a live span")
	}
	if !strings.Contains(serialized, "traceparent") {
		t.Errorf("serialized form %q carries no traceparent", serialized)
	}

	link, ok := LinkFrom(serialized)
	if !ok {
		t.Fatal("LinkFrom could not rebuild a span context from the serialized form")
	}
	if link.SpanContext.TraceID() != original.TraceID() {
		t.Errorf("trace id round-tripped as %s, want %s", link.SpanContext.TraceID(), original.TraceID())
	}
	if link.SpanContext.SpanID() != original.SpanID() {
		t.Errorf("span id round-tripped as %s, want %s", link.SpanContext.SpanID(), original.SpanID())
	}
	if !link.SpanContext.IsSampled() {
		t.Error("sampled flag was lost in the round trip — the linked trace would be dropped")
	}
}

// TestSerializeContext_EmptyWithoutASpan pins the "no context is normal"
// contract the nullable trace_context columns depend on.
func TestSerializeContext_EmptyWithoutASpan(t *testing.T) {
	withRecorder(t)

	if got := SerializeContext(context.Background()); got != "" {
		t.Errorf("SerializeContext(no span) = %q, want empty so the column stores NULL", got)
	}
}

// TestLinkFrom_RejectsUnusableInput covers every way a stored value can
// legitimately be useless: rows predating the column, tracing-disabled
// rows, and corrupted JSON. All must report ok=false rather than
// producing a bogus link or panicking in a background worker.
func TestLinkFrom_RejectsUnusableInput(t *testing.T) {
	withRecorder(t)

	for _, in := range []string{
		"",
		"{}",
		"not json at all",
		`{"traceparent":""}`,
		`{"traceparent":"garbage"}`,
		`{"traceparent":"00-00000000000000000000000000000000-0000000000000000-00"}`,
	} {
		if _, ok := LinkFrom(in); ok {
			t.Errorf("LinkFrom(%q) reported ok, want false", in)
		}
	}
}

// TestStartLinkedRoot_NewTraceWithLink is the decision this sprint turns
// on, asserted rather than merely documented: the async span must NOT be
// a child of the originating request (which ended long ago), and must
// carry a link back to it.
func TestStartLinkedRoot_NewTraceWithLink(t *testing.T) {
	recorder := withRecorder(t)

	requestCtx, requestSpan := startSpan(t, "POST /transfers/")
	serialized := SerializeContext(requestCtx)
	originalTrace := trace.SpanContextFromContext(requestCtx).TraceID()
	requestSpan.End() // the request finishes BEFORE the relay publishes

	_, publishSpan := StartLinkedRoot(context.Background(), "test", "outbox publish", serialized)
	publishSpan.End()

	var publish sdktrace.ReadOnlySpan
	for _, s := range recorder.Ended() {
		if s.Name() == "outbox publish" {
			publish = s
		}
	}
	if publish == nil {
		t.Fatal("publish span was never recorded")
	}

	if publish.SpanContext().TraceID() == originalTrace {
		t.Error("publish span joined the request's trace — it must start a new one, or Jaeger renders a parent whose child begins after it ended")
	}
	if publish.Parent().IsValid() {
		t.Errorf("publish span has parent %s, want none (it is a root)", publish.Parent().SpanID())
	}

	links := publish.Links()
	if len(links) != 1 {
		t.Fatalf("publish span has %d links, want exactly 1 back to the originating trace", len(links))
	}
	if links[0].SpanContext.TraceID() != originalTrace {
		t.Errorf("link points at trace %s, want %s", links[0].SpanContext.TraceID(), originalTrace)
	}
}

// TestStartLinkedRoot_NewRootEvenInsideALiveSpan guards the WithNewRoot
// option specifically. The relay's own loop can carry an ambient span,
// and without WithNewRoot the publish would quietly become its child —
// producing one enormous relay trace with every event ever published
// hanging off it.
func TestStartLinkedRoot_NewRootEvenInsideALiveSpan(t *testing.T) {
	recorder := withRecorder(t)

	ambientCtx, ambientSpan := startSpan(t, "relay tick")
	defer ambientSpan.End()

	_, publishSpan := StartLinkedRoot(ambientCtx, "test", "outbox publish", "")
	publishSpan.End()

	for _, s := range recorder.Ended() {
		if s.Name() == "outbox publish" && s.Parent().IsValid() {
			t.Error("publish span inherited the ambient span as a parent despite WithNewRoot")
		}
	}
}

// TestStartLinkedRoot_WithoutStoredContext covers rows that have no trace
// recorded: the span must still be created (the work happened and is
// worth seeing), just with no link.
func TestStartLinkedRoot_WithoutStoredContext(t *testing.T) {
	recorder := withRecorder(t)

	_, span := StartLinkedRoot(context.Background(), "test", "reconcile transfer", "")
	span.End()

	var found bool
	for _, s := range recorder.Ended() {
		if s.Name() == "reconcile transfer" {
			found = true
			if len(s.Links()) != 0 {
				t.Errorf("span has %d links, want 0 when nothing was stored", len(s.Links()))
			}
		}
	}
	if !found {
		t.Error("span was not recorded at all — work with no stored trace must still be visible")
	}
}

// TestInjectExtractMap covers the Kafka-header hop: what the relay puts on
// the message must be what the consumer continues from.
func TestInjectExtractMap(t *testing.T) {
	withRecorder(t)

	ctx, span := startSpan(t, "outbox publish")
	defer span.End()
	want := trace.SpanContextFromContext(ctx)

	headers := InjectMap(ctx)
	if len(headers) == 0 {
		t.Fatal("InjectMap produced no headers")
	}

	got := trace.SpanContextFromContext(ExtractMap(context.Background(), headers))
	if got.TraceID() != want.TraceID() {
		t.Errorf("trace id across headers = %s, want %s", got.TraceID(), want.TraceID())
	}
	if got.SpanID() != want.SpanID() {
		t.Errorf("span id across headers = %s, want %s — the consumer would not nest under the publish", got.SpanID(), want.SpanID())
	}
}

// TestExtractMap_IgnoresUnrelatedHeaders: every Kafka message also carries
// event_type and any number of other headers. Extraction must tolerate
// them rather than choking on a carrier it did not fully write.
func TestExtractMap_IgnoresUnrelatedHeaders(t *testing.T) {
	withRecorder(t)

	ctx, span := startSpan(t, "outbox publish")
	defer span.End()

	headers := InjectMap(ctx)
	headers["event_type"] = "TransferCompleted"
	headers["dlq_reason"] = "something entirely unrelated"

	got := trace.SpanContextFromContext(ExtractMap(context.Background(), headers))
	if !got.IsValid() {
		t.Error("extraction failed once non-trace headers were present")
	}
}
