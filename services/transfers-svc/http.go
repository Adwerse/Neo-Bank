package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"neobank/pkg/tracing"
	accountsv1 "neobank/proto/gen/go/accounts/v1"
	fraudv1 "neobank/proto/gen/go/fraud/v1"
	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

type createTransferRequest struct {
	RecipientAccountNumber string `json:"recipient_account_number"`
	Amount                 int64  `json:"amount"`
}

// createTransferResponse embeds Transfer directly (so its fields flatten
// into the JSON body) plus an optional Message, populated only when
// settleTransfer couldn't reach a definite outcome.
type createTransferResponse struct {
	Transfer
	Message string `json:"message,omitempty"`
}

// resolveSenderAccountID looks up the calling user's own account_id from the
// gateway-injected X-User-Id header — the sender is always the authenticated
// caller, never a client-supplied value, so a client can never send money
// from an account that isn't theirs. Returns ("", nil) if no account exists
// for this user yet (a real but rare state — see accounts-svc's own GET /me
// 404 for the same case).
func resolveSenderAccountID(ctx context.Context, accountsClient accountsv1.AccountsServiceClient, userID string) (string, error) {
	acc, err := accountsClient.GetAccountByUserID(ctx, &accountsv1.GetAccountByUserIDRequest{UserId: userID})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return "", nil
		}
		return "", fmt.Errorf("resolve sender account: %w", err)
	}
	return acc.GetAccountId(), nil
}

// createTransferHandler is registered at "POST /" — transfers-svc's mux is
// root-relative because the gateway strips the "/transfers" prefix before
// forwarding, same convention as accounts-svc. The sender is always the
// authenticated caller (resolved from X-User-Id, which the gateway injects
// from the JWT and strips from anything the client sent), never taken from
// the request body — a client must never be able to send money "as" someone
// else. Idempotency-Key is a required header, not a body field, matching
// the HTTP convention for this kind of retry-safety token.
func createTransferHandler(pool *pgxpool.Pool, accountsClient accountsv1.AccountsServiceClient, fraudClient fraudv1.FraudServiceClient, ledgerClient ledgerv1.LedgerServiceClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		idempotencyKey := r.Header.Get("Idempotency-Key")
		if idempotencyKey == "" {
			writeJSONError(w, http.StatusBadRequest, "missing Idempotency-Key header")
			return
		}

		senderAccountID, err := resolveSenderAccountID(r.Context(), accountsClient, r.Header.Get("X-User-Id"))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		if senderAccountID == "" {
			writeJSONError(w, http.StatusNotFound, "account not found")
			return
		}

		var req createTransferRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		ctx := r.Context()
		// The amount is on the span deliberately — see pkg/tracing's
		// attribute policy for why money is included and account numbers
		// are not. Note req.RecipientAccountNumber is NOT recorded: it is
		// the human-facing identifier, and the resolved internal ids below
		// serve every debugging purpose it would.
		tracing.SetAttributes(ctx,
			tracing.AccountID(senderAccountID),
			tracing.AmountMinor(req.Amount),
		)

		transfer, outcome, err := createTransfer(ctx, pool, accountsClient, idempotencyKey, senderAccountID, req.RecipientAccountNumber, req.Amount)
		if err != nil {
			tracing.Fail(ctx, "create_transfer_failed", err)
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		tracing.SetAttributes(ctx,
			tracing.TransferID(transfer.ID),
			tracing.TransferOutcome(outcome.String()),
		)
		switch outcome {
		case createTransferInvalidAmount:
			writeJSONError(w, http.StatusBadRequest, "invalid amount")
			return
		case createTransferRecipientNotFound:
			writeJSONError(w, http.StatusNotFound, "recipient not found")
			return
		case createTransferSelfTransfer:
			writeJSONError(w, http.StatusBadRequest, "cannot transfer to your own account")
			return
		case createTransferRecipientClosed:
			writeJSONError(w, http.StatusConflict, "recipient account is closed")
			return
		case createTransferSenderNotActive:
			writeJSONError(w, http.StatusConflict, "sender account is not active")
			return
		case createTransferKeyReused:
			writeJSONError(w, http.StatusUnprocessableEntity, "idempotency key already used with different parameters")
			return
		case createTransferReplayed:
			// A prior request with this exact key already ran — return its
			// current state as-is, without re-triggering settleTransfer.
			// That's the whole point: a replay must never call ledger twice.
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(createTransferResponse{Transfer: transfer})
			return
		}

		// Must stay strictly after the outcome switch's early returns above
		// (createTransferReplayed included) — a replay must never trigger a
		// second fraud check, both to avoid re-charging an external check
		// and because a repeat call would itself feed fraud-svc's own
		// velocity counters.
		transfer, fraudOutcome, err := checkTransferFraud(ctx, pool, fraudClient, transfer)
		if err != nil {
			tracing.Fail(ctx, "fraud_check_failed", err)
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		tracing.SetAttributes(ctx, tracing.FraudDecision(fraudOutcome.String()))
		switch fraudOutcome {
		case fraudCheckRejected:
			// This is the branch the tracing sprint exists to make legible:
			// a rejected transfer's trace ends here, with no ledger-svc
			// span below it, and the rule that stopped it named on the
			// span. failure_reason holds fraud-svc's triggered rule (see
			// markTransferRejected) — recording it here is what turns "show
			// me everything the velocity rule blocked" into a Jaeger query
			// against transfers-svc, without having to join to fraud-svc's
			// own spans.
			//
			// Not marked as a span error, for the same reason fraud-svc
			// does not mark it: the system worked correctly. The status and
			// rule attributes carry the news.
			tracing.SetAttributes(ctx, tracing.TransferStatus(transfer.Status))
			if transfer.FailureReason != nil && *transfer.FailureReason != "" {
				tracing.SetAttributes(ctx, tracing.FraudTriggeredRule(*transfer.FailureReason))
			}
			// Matches "failed" also returning 201 below: the transfer
			// resource was created either way, the JSON body's status
			// field carries the actual news.
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(createTransferResponse{Transfer: transfer})
			return
		case fraudCheckUncertain:
			// A real degradation, unlike a rejection: fraud-svc could not
			// be reached or gave an answer we don't understand, and the
			// transfer is parked in pending as a result. Marked as an error
			// so it shows up when filtering for problems.
			tracing.Fail(ctx, "fraud_check_unavailable", nil)
			tracing.SetAttributes(ctx, tracing.TransferStatus(transfer.Status))
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(createTransferResponse{
				Transfer: transfer,
				Message:  "fraud check unavailable, transfer still pending",
			})
			return
		}

		settled, settleOutcome, err := settleTransfer(ctx, pool, ledgerClient, transfer)
		if err != nil {
			tracing.Fail(ctx, "settlement_failed", err)
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		tracing.SetAttributes(ctx, tracing.TransferStatus(settled.Status))
		if settled.LedgerTransactionID != nil && *settled.LedgerTransactionID != "" {
			tracing.SetAttributes(ctx, tracing.LedgerTransactionID(*settled.LedgerTransactionID))
		}
		if settleOutcome == settlementUncertain {
			// The transfer may or may not have posted — the one outcome
			// worth being able to find every instance of, since it is what
			// the reconciliation worker later has to resolve.
			tracing.Fail(ctx, "settlement_uncertain", nil)
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(createTransferResponse{
				Transfer: settled,
				Message:  "transfer status unknown, still processing",
			})
			return
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(createTransferResponse{Transfer: settled})
	}
}

type createDepositRequest struct {
	Amount int64 `json:"amount"`
}

// createDepositResponse deliberately exposes only deposit_id and
// client_secret — never the Stripe secret key (never held anywhere near
// this response), and nothing else off the Stripe PaymentIntent object
// either. client_secret is the one piece of Stripe-issued data the
// frontend needs to hand to Stripe.js to confirm the payment directly with
// Stripe; everything else about the deposit's state is queryable through
// the deposit resource itself once that exists.
type createDepositResponse struct {
	DepositID    string `json:"deposit_id"`
	ClientSecret string `json:"client_secret"`
}

// createDepositHandler is registered at "POST /deposits" — internal-only
// for now, not yet routed through the gateway (see README). account_id is
// always the authenticated caller's own account (resolved from
// X-User-Id), never taken from the request body — same reasoning as
// createTransferHandler's sender resolution.
func createDepositHandler(pool *pgxpool.Pool, accountsClient accountsv1.AccountsServiceClient, paymentIntents stripePaymentIntentCreator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		accountID, err := resolveSenderAccountID(r.Context(), accountsClient, r.Header.Get("X-User-Id"))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		if accountID == "" {
			writeJSONError(w, http.StatusNotFound, "account not found")
			return
		}

		var req createDepositRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		tracing.SetAttributes(r.Context(),
			tracing.AccountID(accountID),
			tracing.AmountMinor(req.Amount),
		)

		deposit, clientSecret, outcome, err := createDeposit(r.Context(), pool, accountsClient, paymentIntents, accountID, req.Amount)
		if err != nil {
			tracing.Fail(r.Context(), "create_deposit_failed", err)
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		// deposit.ID only. clientSecret is deliberately absent from the
		// span despite being right here in scope: it authorises confirming
		// the payment with Stripe, which makes it a credential, and
		// pkg/tracing's policy keeps credentials out of traces.
		tracing.SetAttributes(r.Context(), tracing.DepositID(deposit.ID))
		switch outcome {
		case createDepositInvalidAmount:
			writeJSONError(w, http.StatusBadRequest, "invalid amount")
			return
		case createDepositAccountNotActive:
			writeJSONError(w, http.StatusConflict, "account is not active")
			return
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(createDepositResponse{DepositID: deposit.ID, ClientSecret: clientSecret})
	}
}

// getDepositHandler is registered at "GET /deposits/{id}" — polled by the
// frontend after creating a deposit to learn its status: 'succeeded' means
// Stripe confirmed the card charge, 'credited' means the ledger balance
// actually reflects it (see deposit_reconcile.go). A client-side
// confirmPayment success only ever tells the frontend the former; this
// endpoint is the only honest source for the latter.
//
// Returns 404 for both "no such deposit" and "this deposit belongs to a
// different account" — same non-distinguishing treatment as everywhere
// else in this codebase (e.g. createTransferHandler's recipient lookup)
// so a client can never use this endpoint to probe which deposit ids
// exist for someone else's account.
func getDepositHandler(pool *pgxpool.Pool, accountsClient accountsv1.AccountsServiceClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		accountID, err := resolveSenderAccountID(r.Context(), accountsClient, r.Header.Get("X-User-Id"))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		if accountID == "" {
			writeJSONError(w, http.StatusNotFound, "account not found")
			return
		}

		deposit, found, err := getDepositByID(r.Context(), pool, r.PathValue("id"))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		if !found || deposit.AccountID != accountID {
			writeJSONError(w, http.StatusNotFound, "deposit not found")
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(deposit)
	}
}

type createWithdrawalRequest struct {
	Amount int64 `json:"amount"`
}

// createWithdrawalHandler is registered at "POST /withdrawals" — a
// SIMULATION, not a real payout; see createWithdrawal's doc comment
// (withdrawal.go) and README. account_id is always the authenticated
// caller's own account, same reasoning as every other handler in this
// file.
func createWithdrawalHandler(pool *pgxpool.Pool, accountsClient accountsv1.AccountsServiceClient, ledgerClient ledgerv1.LedgerServiceClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		accountID, err := resolveSenderAccountID(r.Context(), accountsClient, r.Header.Get("X-User-Id"))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		if accountID == "" {
			writeJSONError(w, http.StatusNotFound, "account not found")
			return
		}

		var req createWithdrawalRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		withdrawal, outcome, err := createWithdrawal(r.Context(), pool, accountsClient, ledgerClient, accountID, req.Amount)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		switch outcome {
		case createWithdrawalInvalidAmount:
			writeJSONError(w, http.StatusBadRequest, "invalid amount")
			return
		case createWithdrawalAccountNotActive:
			writeJSONError(w, http.StatusConflict, "account is not active")
			return
		}

		// 201 for both payout_simulated and failed (insufficient funds) —
		// the withdrawal attempt was recorded either way, same convention
		// as createTransferHandler: the JSON body's status field carries
		// the actual news, not the HTTP status code.
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(withdrawal)
	}
}

// listTransfersResponse is the GET /transfers envelope: a page of entries
// plus an opaque cursor for the next page. NextCursor is omitted once
// there's nothing more to fetch.
type listTransfersResponse struct {
	Transfers  []historyEntry `json:"transfers"`
	NextCursor *string        `json:"next_cursor,omitempty"`
}

// listTransfersHandler is registered at "GET /" (external GET /transfers).
// Despite the name/path, the response is the caller's UNIFIED operations
// feed — transfers, deposits, and withdrawals interleaved by time (see
// getOperationHistoryPage, history.go) — not transfers alone; history.go's
// historyEntry doc comment explains why the name stayed as-is.
//
// Pagination is cursor-based (?cursor=, an opaque token from a prior
// response's next_cursor) rather than offset-based: an offset shifts and
// duplicates rows as new entries are inserted between page requests on a
// growing history, which a keyset cursor over (created_at, id) doesn't.
// limit is still defaulted/clamped rather than rejected — it's just a
// page-size preference, not a security-sensitive input; a malformed
// cursor, in contrast, is rejected with 400 (see errInvalidCursor).
// listTransfersHandler takes both pools and picks per-request, not per-
// deployment: the first page (cursor == nil) is what a client reads
// immediately after making a transfer or deposit — read-your-writes
// matters there, so it stays on writePool (the primary). Paging further
// back (cursor set) is by definition looking at rows the caller already
// scrolled past once; a few seconds of replica lag on THAT page is
// invisible to them, which is what makes it the one read in this codebase
// actually eligible for readPool (see README's read/write split rule —
// this is deliberately not "GET /transfers reads from the replica",
// that's exactly the blanket rule the task warns against).
func listTransfersHandler(writePool, readPool *pgxpool.Pool, accountsClient accountsv1.AccountsServiceClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		accountID, err := resolveSenderAccountID(r.Context(), accountsClient, r.Header.Get("X-User-Id"))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		if accountID == "" {
			writeJSONError(w, http.StatusNotFound, "account not found")
			return
		}

		limit, cursor, err := parseHistoryQuery(r)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid cursor")
			return
		}

		pool := writePool
		if cursor != nil {
			pool = readPool
		}

		entries, hasMore, err := getOperationHistoryPage(r.Context(), pool, accountsClient, accountID, limit, cursor)
		if err != nil {
			if errors.Is(err, errInvalidCursor) {
				writeJSONError(w, http.StatusBadRequest, "invalid cursor")
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}

		resp := listTransfersResponse{Transfers: entries}
		if hasMore && len(entries) > 0 {
			last := entries[len(entries)-1]
			next := encodeCursor(pageCursor{CreatedAt: last.CreatedAt, ID: last.ID})
			resp.NextCursor = &next
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	}
}

func parseHistoryQuery(r *http.Request) (limit int32, cursor *pageCursor, err error) {
	const defaultLimit, maxLimit = 20, 100
	limit = defaultLimit
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= maxLimit {
		limit = int32(v)
	}
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := decodeCursor(raw)
		if err != nil {
			return 0, nil, err
		}
		cursor = &c
	}
	return limit, cursor, nil
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
