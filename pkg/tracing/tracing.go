// Package tracing is the one place this repo configures OpenTelemetry.
//
// The problem it solves: a transfer touches gateway → transfers-svc →
// fraud-svc → ledger-svc, and until now the only way to connect those
// four services' log lines was to line up timestamps by hand and hope no
// two requests overlapped. A trace id propagated across every hop turns
// that into one clickable timeline.
//
// Every service calls Init(ctx, "<its own name>") once in main() and
// defers the returned shutdown. Everything else — HTTP, gRPC, Postgres —
// is automatic instrumentation that reads the global TracerProvider this
// sets up, so a service that forgets to call Init does not crash: it
// silently produces no traces, because the OTel API's default global
// provider is a no-op. That is also exactly what tests get, which is
// deliberate — see Init's doc comment.
package tracing

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	// The semconv version MUST match the one resource.Default() was built
	// with (sdk/resource/builtin.go), currently v1.43.0. resource.Merge
	// refuses to merge two resources carrying different schema URLs, and
	// returns an error rather than picking one:
	//
	//   conflicting Schema URL: https://opentelemetry.io/schemas/1.43.0
	//   and https://opentelemetry.io/schemas/1.26.0
	//
	// which surfaced here as every service logging "tracing disabled" and
	// Jaeger knowing about exactly one service — itself. Bumping the OTel
	// SDK can therefore silently break tracing at runtime; if that error
	// reappears, this import is what needs to move.
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// defaultEndpoint is where Jaeger's OTLP/gRPC receiver listens inside the
// compose network. Overridable with the standard OTEL_EXPORTER_OTLP_ENDPOINT
// so nothing here invents its own configuration vocabulary.
const defaultEndpoint = "jaeger:4317"

// exporterTimeout bounds a single export attempt. Tracing is diagnostics:
// it must never be the reason a request is slow, and a wedged collector
// must not accumulate goroutines.
const exporterTimeout = 5 * time.Second

// shutdownTimeout bounds the final flush on exit. Long enough to get the
// last batch out, short enough not to hold up a container stop.
const shutdownTimeout = 5 * time.Second

// Init configures the global TracerProvider and text-map propagator, and
// returns a shutdown function that flushes whatever is still buffered.
//
// serviceName lands in the resource as service.name and is what Jaeger
// labels every node of the dependency graph with, so it must be the real
// service name ("transfers-svc") rather than something generic — a
// diagram where four services all call themselves "app" is worse than no
// diagram.
//
// Not calling Init is a supported state, not a bug: the OTel API ships a
// no-op global provider, so every otelhttp/otelgrpc/otelpgx hook in the
// codebase becomes a cheap non-recording span. That is what the test
// suite runs with — tests build handlers directly rather than going
// through main(), so they never point an exporter at a Jaeger that isn't
// there and never spend the test run retrying a failed export.
//
// Setting OTEL_SDK_DISABLED=true gets the same no-op behaviour explicitly,
// for running the compose stack without Jaeger.
func Init(ctx context.Context, serviceName string) (func(context.Context) error, error) {
	if os.Getenv("OTEL_SDK_DISABLED") == "true" {
		return func(context.Context) error { return nil }, nil
	}

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultEndpoint
	}

	// WithInsecure because this is plaintext gRPC inside the compose
	// network, matching how every other service-to-service hop here
	// speaks. A real deployment terminates TLS at the collector.
	//
	// No WithDialOption(grpc.WithBlock()): the exporter connects lazily
	// and retries, so a Jaeger that is slow to start (or absent) delays
	// nothing at boot. A service must not fail to start because its
	// diagnostics backend isn't up.
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithTimeout(exporterTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("create otlp exporter for %s: %w", endpoint, err)
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(serviceName),
			// deployment.environment.name — semconv renamed this from the
			// older deployment.environment, and only the key constant is
			// generated for it, hence the explicit .String() rather than a
			// helper function.
			semconv.DeploymentEnvironmentNameKey.String(envOr("DEPLOYMENT_ENV", "local")),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build otel resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		// AlwaysSample: 100% of traces, which is right for local
		// development and wrong for production — see README. At real
		// volume this is a cost and a storage problem, and the usual
		// answer is ParentBased(TraceIDRatioBased(x)) plus a tail sampler
		// that keeps every error regardless of the ratio. The sampling
		// decision is made once, at the root (gateway), and propagated in
		// the traceparent sampled flag: ParentBased below is what makes
		// downstream services honour it instead of each rolling their own
		// dice and producing half-sampled, useless traces.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)
	otel.SetTracerProvider(provider)

	// W3C traceparent/tracestate, and nothing else. This is what carries
	// the trace across every process boundary — otelhttp and otelgrpc
	// both inject and extract through this global propagator, so
	// forgetting to set it produces a disconnected span per service
	// rather than one trace, which is the single most common way a
	// tracing setup silently does nothing useful.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()
		return provider.Shutdown(ctx)
	}, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Fail marks the span in ctx as failed and records what kind of failure
// it was.
//
// The errorType argument is the point, and the reason this is a helper
// rather than a bare span.RecordError call. Jaeger can filter on span
// attributes, so a consistent, low-cardinality error.type turns "show me
// every transfer that failed fraud" or "every settlement whose ledger
// call timed out" into a query instead of a log grep. err.Error() is
// recorded too, but it is free-form text and useless to filter on.
//
// Safe to call when ctx carries no span — the no-op span ignores it.
func Fail(ctx context.Context, errorType string, err error) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String(AttrErrorType, errorType))
	span.SetStatus(codes.Error, errorType)
	if err != nil {
		span.RecordError(err)
	}
}

// SetAttributes attaches attributes to the span in ctx, if there is one.
// A thin convenience so call sites do not each repeat the
// SpanFromContext dance.
func SetAttributes(ctx context.Context, attrs ...attribute.KeyValue) {
	trace.SpanFromContext(ctx).SetAttributes(attrs...)
}
