package tracing

import (
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// This file exists because the automatic instrumentation cannot help with
// the asynchronous half of this system, and it is worth being precise
// about why.
//
// Every off-the-shelf Kafka wrapper propagates trace context by injecting
// it into message headers AT PUBLISH TIME, from whatever span is live on
// the publishing goroutine. That works when publishing happens inside the
// request that caused it. Here it does not: transfers-svc writes an event
// row inside the same Postgres transaction as the state change, that
// transaction commits, the HTTP span ends, the response goes back to the
// browser — and only some time later does a completely separate relay
// goroutine, which has never heard of that request, read the row and
// publish it.
//
// By then there is no live span to inject from. The context has to be
// carried across the gap by the only thing that spans it: the database
// row itself. SerializeContext writes it in; LinkFrom reads it back out.

// serializedContext is what actually goes into the trace_context column:
// the W3C fields the propagator produces, as a small JSON object. Stored
// as JSONB so `SELECT trace_context->>'traceparent'` works in psql, which
// turns "which trace does this stuck row belong to?" into one query
// rather than a code change.
type serializedContext map[string]string

// SerializeContext renders the trace context active in ctx into a string
// for storage. Returns "" when there is nothing to record — no active
// span, or tracing disabled — so callers can store NULL and readers can
// treat "no context" as an ordinary, expected state rather than an error.
func SerializeContext(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) == 0 {
		return ""
	}
	encoded, err := json.Marshal(serializedContext(carrier))
	if err != nil {
		// MapCarrier is map[string]string; marshalling it cannot fail.
		// Returning "" rather than panicking keeps a diagnostics concern
		// from ever breaking a money-moving transaction.
		return ""
	}
	return string(encoded)
}

// linkContext turns a stored trace_context back into a SpanContext.
func linkContext(serialized string) (trace.SpanContext, bool) {
	if serialized == "" {
		return trace.SpanContext{}, false
	}
	var carrier serializedContext
	if err := json.Unmarshal([]byte(serialized), &carrier); err != nil {
		return trace.SpanContext{}, false
	}
	extracted := otel.GetTextMapPropagator().Extract(context.Background(), propagation.MapCarrier(carrier))
	sc := trace.SpanContextFromContext(extracted)
	if !sc.IsValid() {
		return trace.SpanContext{}, false
	}
	return sc, true
}

// LinkFrom builds a span link pointing at a stored trace context, with
// optional attributes describing WHY the two are related — "caused this
// event", "resolved this transfer".
//
// Returns ok=false when there is no usable context, which is normal: rows
// written before this column existed, rows written while tracing was
// disabled, and rows written by a background job that had no trace of its
// own all legitimately have nothing here.
func LinkFrom(serialized string, attrs ...attribute.KeyValue) (trace.Link, bool) {
	sc, ok := linkContext(serialized)
	if !ok {
		return trace.Link{}, false
	}
	return trace.Link{SpanContext: sc, Attributes: attrs}, true
}

// StartLinkedRoot starts a span that begins a NEW trace, carrying a link
// back to the trace recorded in `serialized`.
//
// This is the shape the asynchronous work in this repo uses, and choosing
// it over "make the async work a child of the original request" is the
// one genuinely interesting decision in the tracing work. See the README,
// but in short: the originating HTTP span finished in ~50ms and the relay
// publishes seconds later, so parenting would render a 50ms span with a
// child starting three seconds after its parent ended. Jaeger draws that
// literally — a bar with a gap, then a child floating outside it — and
// every duration aggregate over the parent becomes a lie, because a
// parent's duration is supposed to contain its children's.
//
// A link says exactly what is true: this is separate work, causally
// related to that other trace. Jaeger renders it as a navigable reference
// in both directions.
//
// WithNewRoot is what makes it a new trace even when the caller's ctx
// happens to carry a span (the relay's own loop, for instance).
func StartLinkedRoot(ctx context.Context, scope, spanName, serialized string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	opts := []trace.SpanStartOption{
		trace.WithNewRoot(),
		trace.WithAttributes(attrs...),
	}
	if link, ok := LinkFrom(serialized); ok {
		opts = append(opts, trace.WithLinks(link))
	}
	return otel.Tracer(scope).Start(ctx, spanName, opts...)
}

// InjectMap renders the trace context in ctx as a plain map, for carriers
// that are not JSON — Kafka message headers, chiefly.
func InjectMap(ctx context.Context) map[string]string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier
}

// ExtractMap returns a context continuing the trace described by m. Used
// at the top of each consumer's message handling, so its spans join the
// delivery trace the relay started rather than rooting a new one per
// message.
func ExtractMap(ctx context.Context, m map[string]string) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(m))
}
