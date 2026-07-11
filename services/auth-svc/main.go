package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultPort     = "8081"
	defaultSMTPAddr = "mailpit:1025"
	defaultSMTPFrom = "noreply@neobank.local"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}
	smtpAddr := os.Getenv("SMTP_ADDR")
	if smtpAddr == "" {
		smtpAddr = defaultSMTPAddr
	}
	smtpFrom := os.Getenv("SMTP_FROM")
	if smtpFrom == "" {
		smtpFrom = defaultSMTPFrom
	}

	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("auth-svc: failed to create postgres pool: %v", err)
	}
	defer pool.Close()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		json.NewEncoder(w).Encode(map[string]string{"service": "auth-svc"})
	})

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		w.Header().Set("Content-Type", "application/json")
		var result int
		if err := pool.QueryRow(ctx, "SELECT 1").Scan(&result); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "service": "auth-svc"})
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "auth-svc"})
	})

	http.HandleFunc("/register", registerHandler(pool, smtpAddr, smtpFrom))

	log.Printf("auth-svc listening on :%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}
