package tracing

import (
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
)

// GRPCServerOption instruments a gRPC server: every incoming RPC becomes
// a server span, continuing the trace the caller propagated in its
// metadata.
//
// StatsHandler rather than the older UnaryInterceptor form — otelgrpc's
// interceptors are deprecated, and the stats handler is the only one of
// the two that sees streaming RPCs properly. None of this repo's services
// stream today, but picking the API that does not have to be revisited
// costs nothing.
func GRPCServerOption() grpc.ServerOption {
	return grpc.StatsHandler(otelgrpc.NewServerHandler())
}

// GRPCClientOption instruments a gRPC client: every outgoing RPC becomes
// a child span and injects the trace context into the request metadata.
//
// This is the gRPC half of what Transport does for HTTP, and it is what
// makes transfers-svc → fraud-svc → ledger-svc appear as nested spans of
// one trace rather than three unrelated ones.
func GRPCClientOption() grpc.DialOption {
	return grpc.WithStatsHandler(otelgrpc.NewClientHandler())
}
