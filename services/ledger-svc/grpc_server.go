package main

import (
	"context"
	"errors"
	"log"

	ledgerv1 "neobank/proto/gen/go/ledger/v1"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ledgerServer implements ledgerv1.LedgerServiceServer. It is ledger-svc's
// only API surface: an internal, service-to-service gRPC contract with no
// gateway route and no notion of a client identity (X-User-Id or
// otherwise) — the caller (transfers-svc, from sprint 5 onward) is
// responsible for having already authenticated and authorized the request
// before it ever reaches here.
type ledgerServer struct {
	ledgerv1.UnimplementedLedgerServiceServer
	pool *pgxpool.Pool
}

func (s *ledgerServer) GetBalance(ctx context.Context, req *ledgerv1.GetBalanceRequest) (*ledgerv1.GetBalanceResponse, error) {
	balance, err := getBalance(ctx, s.pool, req.GetAccountId())
	if errors.Is(err, ErrLedgerAccountNotFound) {
		return nil, status.Error(codes.NotFound, "ledger account not found")
	}
	if err != nil {
		log.Printf("ledger-svc: GetBalance: %v", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	return &ledgerv1.GetBalanceResponse{Balance: balance}, nil
}

func (s *ledgerServer) ExecuteTransfer(ctx context.Context, req *ledgerv1.ExecuteTransferRequest) (*ledgerv1.ExecuteTransferResponse, error) {
	transactionID, outcome, err := executeTransfer(ctx, s.pool, req.GetFromAccountId(), req.GetToAccountId(), req.GetAmount())
	if err != nil {
		log.Printf("ledger-svc: ExecuteTransfer: %v", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	switch outcome {
	case transferOK:
		return &ledgerv1.ExecuteTransferResponse{TransactionId: transactionID}, nil
	case transferInvalidAmount:
		return nil, status.Error(codes.InvalidArgument, "amount must be positive")
	case transferFromAccountNotFound:
		return nil, status.Error(codes.NotFound, "from account not found")
	case transferToAccountNotFound:
		return nil, status.Error(codes.NotFound, "to account not found")
	case transferInsufficientFunds:
		return nil, status.Error(codes.FailedPrecondition, "insufficient funds")
	default:
		log.Printf("ledger-svc: ExecuteTransfer: unhandled outcome %v", outcome)
		return nil, status.Error(codes.Internal, "internal error")
	}
}

func (s *ledgerServer) GetHistory(ctx context.Context, req *ledgerv1.GetHistoryRequest) (*ledgerv1.GetHistoryResponse, error) {
	entries, err := getHistory(ctx, s.pool, req.GetAccountId(), req.GetLimit(), req.GetOffset())
	if errors.Is(err, ErrLedgerAccountNotFound) {
		return nil, status.Error(codes.NotFound, "ledger account not found")
	}
	if errors.Is(err, ErrInvalidPagination) {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err != nil {
		log.Printf("ledger-svc: GetHistory: %v", err)
		return nil, status.Error(codes.Internal, "internal error")
	}

	resp := &ledgerv1.GetHistoryResponse{Entries: make([]*ledgerv1.Entry, 0, len(entries))}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, &ledgerv1.Entry{
			TransactionId: e.TransactionID,
			Amount:        e.Amount,
			CreatedAt:     timestamppb.New(e.CreatedAt),
		})
	}
	return resp, nil
}
