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

		transfer, outcome, err := createTransfer(r.Context(), pool, accountsClient, idempotencyKey, senderAccountID, req.RecipientAccountNumber, req.Amount)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
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
		transfer, fraudOutcome, err := checkTransferFraud(r.Context(), pool, fraudClient, transfer)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		switch fraudOutcome {
		case fraudCheckRejected:
			// Matches "failed" also returning 201 below: the transfer
			// resource was created either way, the JSON body's status
			// field carries the actual news.
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(createTransferResponse{Transfer: transfer})
			return
		case fraudCheckUncertain:
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(createTransferResponse{
				Transfer: transfer,
				Message:  "fraud check unavailable, transfer still pending",
			})
			return
		}

		settled, settleOutcome, err := settleTransfer(r.Context(), pool, ledgerClient, transfer)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		if settleOutcome == settlementUncertain {
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

		deposit, clientSecret, outcome, err := createDeposit(r.Context(), pool, accountsClient, paymentIntents, accountID, req.Amount)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
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

// transferHistoryEntry enriches a raw Transfer with which side of it
// accountID was on and the counterparty's human-readable account_number —
// the recipient was always identified by account_number throughout this
// project, so history should show that too, not a bare account_id UUID.
type transferHistoryEntry struct {
	Transfer
	Direction                 string `json:"direction"` // "outgoing" | "incoming"
	CounterpartyAccountNumber string `json:"counterparty_account_number"`
}

// listTransfersResponse is the GET /transfers envelope: a page of entries
// plus an opaque cursor for the next page. NextCursor is omitted once
// there's nothing more to fetch.
type listTransfersResponse struct {
	Transfers  []transferHistoryEntry `json:"transfers"`
	NextCursor *string                `json:"next_cursor,omitempty"`
}

// listTransfersHandler is registered at "GET /" (external GET /transfers).
// Pagination is cursor-based (?cursor=, an opaque token from a prior
// response's next_cursor) rather than offset-based: an offset shifts and
// duplicates rows as new transfers are inserted between page requests on a
// growing history, which a keyset cursor over (created_at, id) doesn't.
// limit is still defaulted/clamped rather than rejected — it's just a
// page-size preference, not a security-sensitive input; a malformed
// cursor, in contrast, is rejected with 400 (see errInvalidCursor).
func listTransfersHandler(pool *pgxpool.Pool, accountsClient accountsv1.AccountsServiceClient) http.HandlerFunc {
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

		transfers, hasMore, err := getTransferHistoryPage(r.Context(), pool, accountID, limit, cursor)
		if err != nil {
			if errors.Is(err, errInvalidCursor) {
				writeJSONError(w, http.StatusBadRequest, "invalid cursor")
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}

		// Collect every unique counterparty for this page and resolve all
		// of them in a single batch RPC — one call per request, not one
		// call per counterparty.
		counterpartyIDSet := make(map[string]struct{}, len(transfers))
		for _, t := range transfers {
			if t.SenderAccountID == accountID {
				counterpartyIDSet[t.RecipientAccountID] = struct{}{}
			} else {
				counterpartyIDSet[t.SenderAccountID] = struct{}{}
			}
		}
		counterpartyIDs := make([]string, 0, len(counterpartyIDSet))
		for id := range counterpartyIDSet {
			counterpartyIDs = append(counterpartyIDs, id)
		}

		accountNumbers := map[string]string{}
		if len(counterpartyIDs) > 0 {
			resolved, err := accountsClient.ResolveAccountsByIds(r.Context(), &accountsv1.ResolveAccountsByIdsRequest{AccountIds: counterpartyIDs})
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, "failed to process request")
				return
			}
			for _, acc := range resolved.GetAccounts() {
				accountNumbers[acc.GetAccountId()] = acc.GetAccountNumber()
			}
		}

		entries := make([]transferHistoryEntry, 0, len(transfers))
		for _, t := range transfers {
			entry := transferHistoryEntry{Transfer: t}
			counterpartyID := t.RecipientAccountID
			if t.SenderAccountID == accountID {
				entry.Direction = "outgoing"
			} else {
				entry.Direction = "incoming"
				counterpartyID = t.SenderAccountID
			}
			entry.CounterpartyAccountNumber = accountNumbers[counterpartyID]
			entries = append(entries, entry)
		}

		resp := listTransfersResponse{Transfers: entries}
		if hasMore && len(transfers) > 0 {
			last := transfers[len(transfers)-1]
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
