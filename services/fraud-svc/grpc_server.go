package main

import (
	"context"
	"log"

	"neobank/pkg/tracing"
	fraudv1 "neobank/proto/gen/go/fraud/v1"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fraudServer implements fraudv1.FraudServiceServer. It is fraud-svc's only
// API surface: an internal, service-to-service gRPC contract with no
// gateway route — not yet called by anything (integrating a caller is a
// following step).
type fraudServer struct {
	fraudv1.UnimplementedFraudServiceServer
	pool *pgxpool.Pool
}

func (s *fraudServer) CheckTransfer(ctx context.Context, req *fraudv1.CheckTransferRequest) (*fraudv1.CheckTransferResponse, error) {
	// Attached before the work, so they are present even on the error
	// path below. otelgrpc has already created the server span this
	// decorates; there is no manual span to start.
	tracing.SetAttributes(ctx,
		tracing.TransferID(req.GetTransferId()),
		tracing.AccountID(req.GetAccountId()),
		tracing.AmountMinor(req.GetAmount()),
	)

	decision, triggeredRule, reason, err := checkTransfer(ctx, s.pool, req.GetTransferId(), req.GetAccountId(), req.GetAmount())
	if err != nil {
		// Fail closed: a scoring failure is a gRPC error, never a silent
		// approve. What the caller does about an unavailable fraud-svc is
		// its own decision, not something fraud-svc papers over.
		log.Printf("fraud-svc: CheckTransfer: %v", err)
		tracing.Fail(ctx, "fraud_scoring_failed", err)
		return nil, status.Error(codes.Internal, "internal error")
	}

	tracing.SetAttributes(ctx, tracing.FraudDecision(decision))
	if triggeredRule != "" {
		tracing.SetAttributes(ctx, tracing.FraudTriggeredRule(triggeredRule))
	}
	// A rejection is NOT marked as a span error. It is a correct,
	// intentional outcome — the service did exactly its job — and marking
	// it error would put every blocked transfer in the same Jaeger bucket
	// as genuine failures, which is precisely the distinction someone
	// filtering for problems needs kept. The decision attribute is what
	// makes "show me everything fraud rejected" queryable, without
	// claiming anything broke.

	return &fraudv1.CheckTransferResponse{
		Decision:      decision,
		TriggeredRule: triggeredRule,
		Reason:        reason,
	}, nil
}
