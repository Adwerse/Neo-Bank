package main

import (
	"context"
	"log"

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
	decision, triggeredRule, reason, err := checkTransfer(ctx, s.pool, req.GetTransferId(), req.GetAccountId(), req.GetAmount())
	if err != nil {
		// Fail closed: a scoring failure is a gRPC error, never a silent
		// approve. What the caller does about an unavailable fraud-svc is
		// its own decision, not something fraud-svc papers over.
		log.Printf("fraud-svc: CheckTransfer: %v", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	return &fraudv1.CheckTransferResponse{
		Decision:      decision,
		TriggeredRule: triggeredRule,
		Reason:        reason,
	}, nil
}
