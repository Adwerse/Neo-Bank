package main

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	accountsv1 "neobank/proto/gen/go/accounts/v1"
	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

type createTransferRequest struct {
	SenderAccountID        string `json:"sender_account_id"`
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

// createTransferHandler is registered at "POST /" — transfers-svc's mux is
// root-relative because the gateway strips the "/transfers" prefix before
// forwarding, same convention as accounts-svc. sender_account_id is taken
// directly from the request body for now; deriving it from the gateway's
// X-User-Id header instead is a later sprint.
func createTransferHandler(pool *pgxpool.Pool, accountsClient accountsv1.AccountsServiceClient, ledgerClient ledgerv1.LedgerServiceClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var req createTransferRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		idempotencyKey, err := randomUUID()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}

		transfer, outcome, err := createTransfer(r.Context(), pool, accountsClient, idempotencyKey, req.SenderAccountID, req.RecipientAccountNumber, req.Amount)
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

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
