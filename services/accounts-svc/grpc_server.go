package main

import (
	"context"
	"log"

	accountsv1 "neobank/proto/gen/go/accounts/v1"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// accountsServer implements accountsv1.AccountsServiceServer. It is
// accounts-svc's internal, service-to-service gRPC surface, separate from
// (and never touching the routes of) its existing public HTTP API — the
// only intended caller is transfers-svc.
type accountsServer struct {
	accountsv1.UnimplementedAccountsServiceServer
	pool *pgxpool.Pool
}

func (s *accountsServer) ResolveAccountByNumber(ctx context.Context, req *accountsv1.ResolveAccountByNumberRequest) (*accountsv1.ResolveAccountByNumberResponse, error) {
	acc, found, err := getAccountByAccountNumber(ctx, s.pool, req.GetAccountNumber())
	if err != nil {
		log.Printf("accounts-svc: ResolveAccountByNumber: %v", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	if !found {
		return nil, status.Error(codes.NotFound, "account not found")
	}
	return &accountsv1.ResolveAccountByNumberResponse{AccountId: acc.ID, Status: acc.Status}, nil
}

func (s *accountsServer) GetAccountByID(ctx context.Context, req *accountsv1.GetAccountByIDRequest) (*accountsv1.GetAccountByIDResponse, error) {
	acc, found, err := getAccountByID(ctx, s.pool, req.GetAccountId())
	if err != nil {
		log.Printf("accounts-svc: GetAccountByID: %v", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	if !found {
		return nil, status.Error(codes.NotFound, "account not found")
	}
	return &accountsv1.GetAccountByIDResponse{AccountId: acc.ID, Status: acc.Status, AccountNumber: acc.AccountNumber}, nil
}

func (s *accountsServer) GetAccountByUserID(ctx context.Context, req *accountsv1.GetAccountByUserIDRequest) (*accountsv1.GetAccountByUserIDResponse, error) {
	acc, found, err := getAccountByUserID(ctx, s.pool, req.GetUserId())
	if err != nil {
		log.Printf("accounts-svc: GetAccountByUserID: %v", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	if !found {
		return nil, status.Error(codes.NotFound, "account not found")
	}
	return &accountsv1.GetAccountByUserIDResponse{AccountId: acc.ID, Status: acc.Status}, nil
}

// ResolveAccountsByIds has no "not found" case — a batch lookup returning
// fewer accounts than ids requested is a normal outcome, not an error.
func (s *accountsServer) ResolveAccountsByIds(ctx context.Context, req *accountsv1.ResolveAccountsByIdsRequest) (*accountsv1.ResolveAccountsByIdsResponse, error) {
	accounts, err := getAccountsByIDs(ctx, s.pool, req.GetAccountIds())
	if err != nil {
		log.Printf("accounts-svc: ResolveAccountsByIds: %v", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	resp := &accountsv1.ResolveAccountsByIdsResponse{Accounts: make([]*accountsv1.AccountSummary, 0, len(accounts))}
	for _, acc := range accounts {
		resp.Accounts = append(resp.Accounts, &accountsv1.AccountSummary{AccountId: acc.ID, AccountNumber: acc.AccountNumber})
	}
	return resp, nil
}
