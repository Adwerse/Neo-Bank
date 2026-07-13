package main

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
)

func meAccountHandler(pool *pgxpool.Pool) http.HandlerFunc {
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

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(acc)
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
