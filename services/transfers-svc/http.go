package main

import (
	"context"
	"encoding/json"
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

// transferHistoryEntry enriches a raw Transfer with which side of it
// accountID was on and the counterparty's human-readable account_number —
// the recipient was always identified by account_number throughout this
// project, so history should show that too, not a bare account_id UUID.
type transferHistoryEntry struct {
	Transfer
	Direction                 string `json:"direction"` // "sent" | "received"
	CounterpartyAccountNumber string `json:"counterparty_account_number"`
}

// listTransfersHandler is registered at "GET /" (external GET /transfers).
// Pagination is via ?limit=&offset=, defaulted/clamped rather than
// rejected — this is the caller's own history, not a security-sensitive
// input.
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

		limit, offset := parsePagination(r)

		transfers, err := getTransfersForAccount(r.Context(), pool, accountID, limit, offset)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}

		// Memoized per request: the same counterparty can appear in
		// multiple rows on one page.
		accountNumberCache := map[string]string{}
		entries := make([]transferHistoryEntry, 0, len(transfers))
		for _, t := range transfers {
			entry := transferHistoryEntry{Transfer: t}
			counterpartyID := t.RecipientAccountID
			if t.SenderAccountID == accountID {
				entry.Direction = "sent"
			} else {
				entry.Direction = "received"
				counterpartyID = t.SenderAccountID
			}
			number, ok := accountNumberCache[counterpartyID]
			if !ok {
				acc, err := accountsClient.GetAccountByID(r.Context(), &accountsv1.GetAccountByIDRequest{AccountId: counterpartyID})
				if err != nil {
					writeJSONError(w, http.StatusInternalServerError, "failed to process request")
					return
				}
				number = acc.GetAccountNumber()
				accountNumberCache[counterpartyID] = number
			}
			entry.CounterpartyAccountNumber = number
			entries = append(entries, entry)
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(entries)
	}
}

func parsePagination(r *http.Request) (limit, offset int32) {
	const defaultLimit, maxLimit = 20, 100
	limit = defaultLimit
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= maxLimit {
		limit = int32(v)
	}
	if v, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && v >= 0 {
		offset = int32(v)
	}
	return limit, offset
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
