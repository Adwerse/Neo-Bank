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
	defaultPort                = "8086"
	defaultKafkaBrokers        = "kafka:9092"
	defaultUserEventsTopic     = "user.events"
	defaultAccountEventsTopic  = "account.events"
	defaultTransferEventsTopic = "transfer.events"
	// Identical to auth-svc's defaults on purpose: both services should
	// land in the same Mailpit inbox from the same sender, and switching
	// to a real provider should be one pair of env vars, not two.
	defaultSMTPAddr = "mailpit:1025"
	defaultSMTPFrom = "noreply@neobank.local"
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
	transferEventsTopic := os.Getenv("KAFKA_TRANSFER_EVENTS_TOPIC")
	if transferEventsTopic == "" {
		transferEventsTopic = defaultTransferEventsTopic
	}
	smtpAddr := os.Getenv("SMTP_ADDR")
	if smtpAddr == "" {
		smtpAddr = defaultSMTPAddr
	}
	smtpFrom := os.Getenv("SMTP_FROM")
	if smtpFrom == "" {
		smtpFrom = defaultSMTPFrom
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

	// Three independent readers, one per topic (kafka-go's Reader
	// subscribes to exactly one topic), all in the same consumer group.
	// The projection/notification split is not cosmetic — the two
	// constructors differ in StartOffset, and getting that wrong on
	// transfer.events means mailing every user about their entire
	// transfer history. See newProjectionReader's doc comment.
	userEventsReader := newProjectionReader(kafkaBrokers, userEventsTopic)
	defer userEventsReader.Close()
	accountEventsReader := newProjectionReader(kafkaBrokers, accountEventsTopic)
	defer accountEventsReader.Close()
	transferEventsReader := newNotificationReader(kafkaBrokers, transferEventsTopic)
	defer transferEventsReader.Close()

	go runUserActivatedConsumer(context.Background(), userEventsReader, pool)
	go runAccountCreatedConsumer(context.Background(), accountEventsReader, pool)
	go runTransferEventsConsumer(context.Background(), transferEventsReader, pool, smtpAddr, smtpFrom)

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
