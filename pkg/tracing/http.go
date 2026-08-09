package tracing

import (
	"net/http"
	"regexp"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// uuidSegment matches a path segment that is a UUID, so span names can
// collapse /deposits/9a1c2d3e-... down to /deposits/{id}.
//
// Without this, every deposit lookup is its own distinct operation name
// in Jaeger and the operation dropdown becomes an unusable list of
// thousands of one-off entries — the classic high-cardinality span-name
// mistake. otelhttp cannot do this for us: it wraps the mux from the
// outside, so at span-creation time no routing has happened yet and the
// matched pattern is not knowable.
var uuidSegment = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// numericSegment catches id-shaped path segments that are not UUIDs.
var numericSegment = regexp.MustCompile(`^\d+$`)

// SpanName renders a request as a stable, low-cardinality operation name:
// "POST /deposits/{id}".
func SpanName(r *http.Request) string {
	segments := strings.Split(r.URL.Path, "/")
	for i, seg := range segments {
		if uuidSegment.MatchString(seg) || numericSegment.MatchString(seg) {
			segments[i] = "{id}"
		}
	}
	return r.Method + " " + strings.Join(segments, "/")
}

// isNoise reports whether a request should not produce a trace at all.
//
// Two categories, for two different reasons.
//
// Health checks: Docker probes /healthz every 5 seconds per service,
// forever, and the frontend polls as well. Left in, they would be the
// overwhelming majority of spans in Jaeger, pushing real traces out of
// the retention window and making the service-dependency diagram
// meaningless. They also carry nothing a trace can add — a health check
// is a `SELECT 1`, and if it fails the container's own status says so.
//
// The WebSocket endpoint: a span lasts as long as the request that
// created it, and a WebSocket request lasts as long as the connection —
// minutes or hours. That is not a useful trace, it is one enormous span
// that never closes, still open when the trace is queried and skewing
// every duration chart it appears in. The events flowing over that
// socket are worth tracing; the socket itself is not.
//
// Note that a filtered request is passed straight through to the next
// handler with the ORIGINAL ResponseWriter (otelhttp does no wrapping on
// this path), which matters here: the WebSocket upgrade needs to type
// assert to http.Hijacker, and skipping the wrapper removes any question
// of whether the instrumentation preserves it.
func isNoise(r *http.Request) bool {
	if r.URL.Path == "/ws" {
		return true
	}
	return r.URL.Path == "/healthz" || strings.HasSuffix(r.URL.Path, "/healthz")
}

// Handler wraps an http.Handler so every request becomes a server span,
// with the incoming W3C traceparent (if any) continued rather than a new
// trace started.
//
// serviceName is used only for the instrumentation scope; the resource's
// service.name set in Init is what Jaeger groups by.
func Handler(h http.Handler, serviceName string) http.Handler {
	return otelhttp.NewHandler(h, serviceName,
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return SpanName(r)
		}),
		otelhttp.WithFilter(func(r *http.Request) bool { return !isNoise(r) }),
	)
}

// Transport wraps an http.RoundTripper so outgoing requests carry the
// current span's traceparent header and produce a client span.
//
// This is the piece the gateway's reverse proxy needs, and the one whose
// absence is the classic way a trace dies at the first hop: a proxy
// copies the request's own headers faithfully, but the gateway is the
// ROOT of the trace, so the header it must forward does not exist on the
// inbound request at all — it has to be injected from the server span
// the gateway just created. Nothing about copying headers can produce it.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(base,
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return SpanName(r)
		}),
	)
}
