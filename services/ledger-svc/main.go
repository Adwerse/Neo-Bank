package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"neobank/pkg/health"
)

const defaultPort = "8083"

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}
	databaseURL := os.Getenv("DATABASE_URL")

	if err := runMigrations(databaseURL); err != nil {
		log.Fatalf("ledger-svc: failed to run migrations: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		log.Fatalf("ledger-svc: failed to create postgres pool: %v", err)
	}
	defer pool.Close()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		json.NewEncoder(w).Encode(map[string]string{"service": "ledger-svc"})
	})
	http.HandleFunc("/healthz", health.Handler("ledger-svc"))

	log.Printf("ledger-svc listening on :%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}
