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

const (
	defaultPort               = "8086"
	defaultKafkaBrokers       = "kafka:9092"
	defaultUserEventsTopic    = "user.events"
	defaultAccountEventsTopic = "account.events"
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
	userEventsTopic := os.Getenv("KAFKA_USER_EVENTS_TOPIC")
	if userEventsTopic == "" {
		userEventsTopic = defaultUserEventsTopic
	}
	accountEventsTopic := os.Getenv("KAFKA_ACCOUNT_EVENTS_TOPIC")
	if accountEventsTopic == "" {
		accountEventsTopic = defaultAccountEventsTopic
	}
	databaseURL := os.Getenv("DATABASE_URL")

	if err := runMigrations(databaseURL); err != nil {
		log.Fatalf("notifications-svc: failed to run migrations: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		log.Fatalf("notifications-svc: failed to create postgres pool: %v", err)
	}
	defer pool.Close()

	// Two independent readers, one per topic (kafka-go's Reader subscribes
	// to exactly one topic), both in the same consumer group — see
	// newKafkaReader's doc comment for why.
	userEventsReader := newKafkaReader(kafkaBrokers, userEventsTopic)
	defer userEventsReader.Close()
	accountEventsReader := newKafkaReader(kafkaBrokers, accountEventsTopic)
	defer accountEventsReader.Close()

	go runUserActivatedConsumer(context.Background(), userEventsReader, pool)
	go runAccountCreatedConsumer(context.Background(), accountEventsReader, pool)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		json.NewEncoder(w).Encode(map[string]string{"service": "notifications-svc"})
	})
	http.HandleFunc("/healthz", health.Handler("notifications-svc"))

	log.Printf("notifications-svc listening on :%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}
