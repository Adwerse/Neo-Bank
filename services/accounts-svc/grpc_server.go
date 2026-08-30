package main

import (
	"context"
	"log"
	"time"

	"neobank/pkg/iban"
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

	// bankCode is this system's own fictitious institution code (see
	// README) — ResolveAccountByIban compares a candidate IBAN's bank code
	// against this to distinguish "wrong bank" from "not found."
	bankCode string
	// resolveRateLimit/resolveRateWindow bound ResolveAccountByIban's
	// per-user attempt rate — see recordResolveAttempt (rate_limit.go).
	resolveRateLimit  int
	resolveRateWindow time.Duration
}

// ResolveAccountByIban distinguishes four outcomes, in this order,
// deliberately: each earlier check is strictly cheaper than the next, so a
// bad-faith or merely malformed request never pays for more than it needs
// to, and the cheapest checks (structural validation) never touch the
// database or the rate limiter at all.
//
//  1. Malformed IBAN (bad length/charset, or a check-digit mismatch) —
//     rejected locally by iban.Validate, no database query and no rate-limit
//     write. This is both the fastest path AND a real, if partial,
//     brute-force mitigation: mod-97-10 rejects roughly 96% of random
//     guesses for free, before they ever reach the parts of this method
//     that cost anything.
//  2. Per-user rate limit — everything from here on is a "structurally
//     plausible" attempt (valid checksum), the narrowed space a
//     determined guesser would actually operate in, so it's what gets
//     metered.
//  3. Wrong bank code — a valid-looking IBAN for an institution that
//     isn't this one. Cheap (a string comparison against s.bankCode), no
//     database query.
//  4. Database lookup by iban — the only step that actually touches
//     Postgres.
func (s *accountsServer) ResolveAccountByIban(ctx context.Context, req *accountsv1.ResolveAccountByIbanRequest) (*accountsv1.ResolveAccountByIbanResponse, error) {
	normalized := iban.Normalize(req.GetIban())
	if err := iban.Validate(normalized); err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid IBAN")
	}

	userID := req.GetUserId()
	if userID == "" {
		// Required, not merely preferred: silently skipping the rate-limit
		// check for a request with no user_id would make an empty field the
		// bypass for the whole control this method exists to enforce.
		return nil, status.Error(codes.InvalidArgument, "user_id is required")
	}
	allowed, err := recordResolveAttempt(ctx, s.pool, userID, s.resolveRateLimit, s.resolveRateWindow)
	if err != nil {
		log.Printf("accounts-svc: ResolveAccountByIban: rate limit check: %v", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	if !allowed {
		return nil, status.Error(codes.ResourceExhausted, "too many resolve attempts, try again later")
	}

	// iban.Validate only enforces the fixed 22-character IE(2)+check
	// digits(2)+bank code(4)+... layout when the country code is "IE" —
	// for any other country it only checks the generic checksum, with no
	// minimum length beyond 4. A non-IE (or otherwise not-our-shape) IBAN
	// is therefore always "not this bank," checked and rejected here
	// BEFORE slicing out a bank code, not after: slicing normalized[4:8]
	// on a validly-checksummed but merely 4-character non-IE string (they
	// exist — e.g. "AT18") would panic.
	if len(normalized) != iban.IELength || normalized[0:2] != iban.CountryIE {
		return nil, status.Error(codes.FailedPrecondition, "only transfers within this bank are supported")
	}
	bankCode := normalized[4:8]
	if bankCode != s.bankCode {
		return nil, status.Error(codes.FailedPrecondition, "only transfers within this bank are supported")
	}

	acc, found, err := getAccountByIBAN(ctx, s.pool, normalized)
	if err != nil {
		log.Printf("accounts-svc: ResolveAccountByIban: %v", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	if !found {
		return nil, status.Error(codes.NotFound, "account_not_found")
	}
	return &accountsv1.ResolveAccountByIbanResponse{AccountId: acc.ID, Status: acc.Status}, nil
}

func (s *accountsServer) GetAccountByID(ctx context.Context, req *accountsv1.GetAccountByIDRequest) (*accountsv1.GetAccountByIDResponse, error) {
	acc, found, err := getAccountByID(ctx, s.pool, req.GetAccountId())
	if err != nil {
		log.Printf("accounts-svc: GetAccountByID: %v", err)
		return nil, status.Error(codes.Internal, "internal error")
	}
	if !found {
		return nil, status.Error(codes.NotFound, "account_not_found")
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
		return nil, status.Error(codes.NotFound, "account_not_found")
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
		resp.Accounts = append(resp.Accounts, &accountsv1.AccountSummary{AccountId: acc.ID, Iban: acc.IBAN})
	}
	return resp, nil
}
