package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

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
	Balance  int64  `json:"balance"` // minor units (cents)
	Currency string `json:"currency"`
}

func meAccountHandler(pool *pgxpool.Pool, ledgerClient ledgerv1.LedgerServiceClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		userID := r.Header.Get("X-User-Id")
		if userID == "" {
			writeJSONError(w, http.StatusBadRequest, "missing_user_id_header")
			return
		}

		acc, found, err := getAccountByUserID(r.Context(), pool, userID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error")
			return
		}
		if !found {
			writeJSONError(w, http.StatusNotFound, "account_not_found")
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
				writeJSONError(w, http.StatusServiceUnavailable, "balance_service_unavailable")
			default:
				// codes.NotFound (the ledger account should exist for any
				// account that has an accounts row, so this is an internal
				// inconsistency, not a client error) and anything else.
				writeJSONError(w, http.StatusInternalServerError, "internal_error")
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

// balanceHistoryPoint is one day's closing balance, oldest first.
type balanceHistoryPoint struct {
	Date    string `json:"date"` // YYYY-MM-DD
	Balance int64  `json:"balance"`
}

type balanceHistoryResponse struct {
	Points []balanceHistoryPoint `json:"points"`
}

// balanceHistoryHandler translates the week/month/all range a chart asks
// for into the `from` cutoff ledger-svc's GetBalanceHistory actually wants
// — ledger-svc itself has no notion of those periods. Same
// never-fake-a-number contract as meAccountHandler: ledger-svc being
// unreachable is a 503, never a 200 with an empty or fabricated series.
func balanceHistoryHandler(pool *pgxpool.Pool, ledgerClient ledgerv1.LedgerServiceClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		userID := r.Header.Get("X-User-Id")
		if userID == "" {
			writeJSONError(w, http.StatusBadRequest, "missing_user_id_header")
			return
		}

		var from *timestamppb.Timestamp
		switch r.URL.Query().Get("range") {
		case "week":
			from = timestamppb.New(time.Now().AddDate(0, 0, -7))
		case "month":
			from = timestamppb.New(time.Now().AddDate(0, -1, 0))
		case "all":
			from = nil
		default:
			writeJSONError(w, http.StatusBadRequest, "invalid_range")
			return
		}

		acc, found, err := getAccountByUserID(r.Context(), pool, userID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error")
			return
		}
		if !found {
			writeJSONError(w, http.StatusNotFound, "account_not_found")
			return
		}

		histResp, err := ledgerClient.GetBalanceHistory(r.Context(), &ledgerv1.GetBalanceHistoryRequest{AccountId: acc.ID, From: from})
		if err != nil {
			switch status.Code(err) {
			case codes.Unavailable, codes.DeadlineExceeded:
				writeJSONError(w, http.StatusServiceUnavailable, "balance_history_unavailable")
			default:
				writeJSONError(w, http.StatusInternalServerError, "internal_error")
			}
			return
		}

		points := make([]balanceHistoryPoint, 0, len(histResp.GetPoints()))
		for _, p := range histResp.GetPoints() {
			points = append(points, balanceHistoryPoint{
				Date:    p.GetDate().AsTime().Format("2006-01-02"),
				Balance: p.GetBalance(),
			})
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(balanceHistoryResponse{Points: points})
	}
}

func getAccountHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		acc, found, err := getAccountByID(r.Context(), pool, r.PathValue("id"))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error")
			return
		}
		if !found {
			writeJSONError(w, http.StatusNotFound, "account_not_found")
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
			writeJSONError(w, http.StatusBadRequest, "invalid_request_body")
			return
		}
		if _, ok := validAccountStatuses[req.Status]; !ok {
			writeJSONError(w, http.StatusBadRequest, "invalid_status_value")
			return
		}

		acc, outcome, err := updateAccountStatus(r.Context(), pool, r.PathValue("id"), req.Status)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error")
			return
		}
		switch outcome {
		case statusUpdateNotFound:
			writeJSONError(w, http.StatusNotFound, "account_not_found")
			return
		case statusUpdateInvalidTransition:
			writeJSONError(w, http.StatusConflict, "invalid_status_transition")
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
