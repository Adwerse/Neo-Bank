package main

import (
	"context"
	"errors"
	"log"
	"time"

	"neobank/pkg/tracing"
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
	tracing.SetAttributes(ctx, tracing.AmountMinor(req.GetAmount()))

	transactionID, outcome, err := executeTransfer(ctx, s.pool, req.GetFromAccountId(), req.GetToAccountId(), req.GetAmount(), req.GetReference())
	if err != nil {
		log.Printf("ledger-svc: ExecuteTransfer: %v", err)
		tracing.Fail(ctx, "ledger_execute_failed", err)
		return nil, status.Error(codes.Internal, "internal error")
	}

	tracing.SetAttributes(ctx, tracing.LedgerOutcome(outcome.String()))
	// insufficient_funds and the not-found outcomes are NOT marked as span
	// errors: they are the ledger correctly refusing to post, which is the
	// system working. Only the err path above — where the ledger could not
	// determine an answer at all — is a failure. Keeping that line where
	// it is means Jaeger's error filter surfaces broken ledgers rather
	// than users with empty accounts.
	switch outcome {
	case transferOK:
		// The id both entries of the double-entry pair share. It is
		// deliberately distinct from the transfer id transfers-svc knows,
		// and having both on one trace is what lets someone follow a
		// single movement of money across the two services' tables.
		tracing.SetAttributes(ctx, tracing.LedgerTransactionID(transactionID))
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

func (s *ledgerServer) Deposit(ctx context.Context, req *ledgerv1.DepositRequest) (*ledgerv1.DepositResponse, error) {
	transactionID, outcome, err := deposit(ctx, s.pool, req.GetAccountId(), req.GetAmount(), req.GetReference())
	if err != nil {
		log.Printf("ledger-svc: Deposit: %v", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	switch outcome {
	case depositOK:
		return &ledgerv1.DepositResponse{TransactionId: transactionID}, nil
	case depositInvalidAmount:
		return nil, status.Error(codes.InvalidArgument, "amount must be positive")
	case depositAccountNotFound:
		return nil, status.Error(codes.NotFound, "account not found")
	default:
		log.Printf("ledger-svc: Deposit: unhandled outcome %v", outcome)
		return nil, status.Error(codes.Internal, "internal error")
	}
}

func (s *ledgerServer) ReverseDeposit(ctx context.Context, req *ledgerv1.DepositRequest) (*ledgerv1.DepositResponse, error) {
	transactionID, outcome, err := reverseDeposit(ctx, s.pool, req.GetAccountId(), req.GetAmount(), req.GetReference())
	if err != nil {
		log.Printf("ledger-svc: ReverseDeposit: %v", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	switch outcome {
	case depositOK:
		return &ledgerv1.DepositResponse{TransactionId: transactionID}, nil
	case depositInvalidAmount:
		return nil, status.Error(codes.InvalidArgument, "amount must be positive")
	case depositAccountNotFound:
		return nil, status.Error(codes.NotFound, "account not found")
	default:
		log.Printf("ledger-svc: ReverseDeposit: unhandled outcome %v", outcome)
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

func (s *ledgerServer) GetTransactionByReference(ctx context.Context, req *ledgerv1.GetTransactionByReferenceRequest) (*ledgerv1.GetTransactionByReferenceResponse, error) {
	transactionID, found, err := getTransactionByReference(ctx, s.pool, req.GetReference())
	if err != nil {
		log.Printf("ledger-svc: GetTransactionByReference: %v", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	return &ledgerv1.GetTransactionByReferenceResponse{Found: found, TransactionId: transactionID}, nil
}

func (s *ledgerServer) GetBalanceHistory(ctx context.Context, req *ledgerv1.GetBalanceHistoryRequest) (*ledgerv1.GetBalanceHistoryResponse, error) {
	var from *time.Time
	if req.GetFrom() != nil {
		t := req.GetFrom().AsTime()
		from = &t
	}

	points, err := getBalanceHistory(ctx, s.pool, req.GetAccountId(), from)
	if errors.Is(err, ErrLedgerAccountNotFound) {
		return nil, status.Error(codes.NotFound, "ledger account not found")
	}
	if err != nil {
		log.Printf("ledger-svc: GetBalanceHistory: %v", err)
		return nil, status.Error(codes.Internal, "internal error")
	}

	resp := &ledgerv1.GetBalanceHistoryResponse{Points: make([]*ledgerv1.BalancePoint, 0, len(points))}
	for _, p := range points {
		resp.Points = append(resp.Points, &ledgerv1.BalancePoint{
			Date:    timestamppb.New(p.Date),
			Balance: p.Balance,
		})
	}
	return resp, nil
}

func (s *ledgerServer) CreateLedgerAccount(ctx context.Context, req *ledgerv1.CreateLedgerAccountRequest) (*ledgerv1.CreateLedgerAccountResponse, error) {
	acc, err := createLedgerAccount(ctx, s.pool, req.GetAccountId())
	if err != nil {
		// A duplicate account_id is not an error here — createLedgerAccount's
		// upsert returns the existing row. Reaching this branch means a
		// genuine DB failure (bad connection, malformed account_id), not a
		// benign redelivery.
		log.Printf("ledger-svc: CreateLedgerAccount: %v", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	return &ledgerv1.CreateLedgerAccountResponse{
		AccountId: acc.AccountID,
		CreatedAt: timestamppb.New(acc.CreatedAt),
	}, nil
}
