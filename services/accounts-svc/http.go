package main

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

// defaultCurrency is the single currency this MVP reports for every balance.
// ledger-svc stores balances as plain integer minor units with no currency
// dimension, so the currency is supplied here rather than by the ledger. The
// balance itself stays an integer in minor units (cents) in the API — turning
// it into "123.45 €" is the frontend's job, not this endpoint's.
const defaultCurrency = "EUR"

// meResponse is the GET /me body: the account plus its ledger balance. It is
// separate from Account (which the other handlers return as-is) precisely
// because only /me carries a balance — the balance comes from a second
// service (ledger-svc), not the accounts row.
type meResponse struct {
	Account
	Balance  int64  `json:"balance"`  // minor units (cents)
	Currency string `json:"currency"`
}

func meAccountHandler(pool *pgxpool.Pool, ledgerClient ledgerv1.LedgerServiceClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		userID := r.Header.Get("X-User-Id")
		if userID == "" {
			writeJSONError(w, http.StatusBadRequest, "missing X-User-Id header")
			return
		}

		acc, found, err := getAccountByUserID(r.Context(), pool, userID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		if !found {
			writeJSONError(w, http.StatusNotFound, "account not found")
			return
		}

		// The balance is authoritative and lives in ledger-svc, keyed by the
		// account id (accounts.id == ledger_accounts.account_id). If ledger is
		// unreachable we return 503 rather than a 200 with balance 0 — showing
		// a fake zero balance in a bank is worse than an honest "unavailable".
		balResp, err := ledgerClient.GetBalance(r.Context(), &ledgerv1.GetBalanceRequest{AccountId: acc.ID})
		if err != nil {
			switch status.Code(err) {
			case codes.Unavailable, codes.DeadlineExceeded:
				// ledger-svc is down or slow — transient, retryable.
				writeJSONError(w, http.StatusServiceUnavailable, "balance service temporarily unavailable")
			default:
				// codes.NotFound (the ledger account should exist for any
				// account that has an accounts row, so this is an internal
				// inconsistency, not a client error) and anything else.
				writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			}
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(meResponse{
			Account:  acc,
			Balance:  balResp.GetBalance(),
			Currency: defaultCurrency,
		})
	}
}

func getAccountHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		acc, found, err := getAccountByID(r.Context(), pool, r.PathValue("id"))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		if !found {
			writeJSONError(w, http.StatusNotFound, "account not found")
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(acc)
	}
}

func updateAccountStatusHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var req struct {
			Status string `json:"status"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if _, ok := validAccountStatuses[req.Status]; !ok {
			writeJSONError(w, http.StatusBadRequest, "invalid status value")
			return
		}

		acc, outcome, err := updateAccountStatus(r.Context(), pool, r.PathValue("id"), req.Status)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		switch outcome {
		case statusUpdateNotFound:
			writeJSONError(w, http.StatusNotFound, "account not found")
			return
		case statusUpdateInvalidTransition:
			writeJSONError(w, http.StatusConflict, "invalid status transition")
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(acc)
	}
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
