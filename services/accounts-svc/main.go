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
	defaultPort         = "8082"
	defaultKafkaBrokers = "kafka:9092"
	defaultKafkaTopic   = "user.events"
	kafkaConsumerGroup  = "accounts-svc"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}
	kafkaBrokers := os.Getenv("KAFKA_BROKERS")
	if kafkaBrokers == "" {
		kafkaBrokers = defaultKafkaBrokers
	}
	kafkaTopic := os.Getenv("KAFKA_TOPIC")
	if kafkaTopic == "" {
		kafkaTopic = defaultKafkaTopic
	}
	databaseURL := os.Getenv("DATABASE_URL")

	if err := runMigrations(databaseURL); err != nil {
		log.Fatalf("accounts-svc: failed to run migrations: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		log.Fatalf("accounts-svc: failed to create postgres pool: %v", err)
	}
	defer pool.Close()

	kafkaReader := newKafkaReader(kafkaBrokers, kafkaTopic, kafkaConsumerGroup)
	defer kafkaReader.Close()

	go runUserActivatedConsumer(context.Background(), kafkaReader, pool)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		json.NewEncoder(w).Encode(map[string]string{"service": "accounts-svc"})
	})

	// Method-qualified (not bare "/healthz") so it doesn't create an
	// unresolvable ambiguity with "GET /{id}" below: an unqualified exact
	// path matches every method, which Go's ServeMux refuses to rank
	// against a method-scoped wildcard at registration time.
	http.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		w.Header().Set("Content-Type", "application/json")
		var result int
		if err := pool.QueryRow(ctx, "SELECT 1").Scan(&result); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "service": "accounts-svc"})
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "accounts-svc"})
	})

	// Root-relative, matching auth-svc's convention: the gateway's reverse
	// proxy strips the "/accounts" prefix before forwarding here (see
	// gateway/proxy.go's newProxy/StripPrefix), so these routes must not
	// repeat it themselves.
	http.HandleFunc("GET /me", meAccountHandler(pool))
	http.HandleFunc("GET /{id}", getAccountHandler(pool))
	http.HandleFunc("PATCH /{id}/status", updateAccountStatusHandler(pool))

	log.Printf("accounts-svc listening on :%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}
