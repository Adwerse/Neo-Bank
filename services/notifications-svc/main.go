package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"neobank/pkg/pgha"
	"neobank/pkg/tracing"
)

const (
	defaultPort                = "8086"
	defaultKafkaBrokers        = "kafka:9092"
	defaultUserEventsTopic     = "user.events"
	defaultAccountEventsTopic  = "account.events"
	defaultTransferEventsTopic = "transfer.events"
	defaultTransferDLQTopic    = "transfer.events.dlq"
	// Identical to auth-svc's defaults on purpose: both services should
	// land in the same Mailpit inbox from the same sender, and switching
	// to a real provider should be one pair of env vars, not two.
	defaultSMTPAddr = "mailpit:1025"
	defaultSMTPFrom = "noreply@neobank.local"

	// shutdownTimeout bounds how long a SIGTERM/SIGINT gives the HTTP
	// server to stop accepting new connections. It does NOT bound the
	// Kafka consumers — see main()'s comment on wg.Wait for why those are
	// left unbounded on purpose.
	shutdownTimeout = 10 * time.Second
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
	transferDLQTopic := os.Getenv("KAFKA_TRANSFER_DLQ_TOPIC")
	if transferDLQTopic == "" {
		transferDLQTopic = defaultTransferDLQTopic
	}
	smtpAddr := os.Getenv("SMTP_ADDR")
	if smtpAddr == "" {
		smtpAddr = defaultSMTPAddr
	}
	smtpFrom := os.Getenv("SMTP_FROM")
	if smtpFrom == "" {
		smtpFrom = defaultSMTPFrom
	}
	// Tracing is set up before anything that could produce a span. A
	// failure is logged rather than fatal: observability going down must
	// not take the service with it — the global provider simply stays the
	// API's no-op and everything else behaves identically.
	shutdownTracing, err := tracing.Init(context.Background(), "notifications-svc")
	if err != nil {
		log.Printf("notifications-svc: tracing disabled: %v", err)
		shutdownTracing = func(context.Context) error { return nil }
	}
	defer func() {
		if err := shutdownTracing(context.Background()); err != nil {
			log.Printf("notifications-svc: tracing shutdown: %v", err)
		}
	}()

	databaseURL := os.Getenv("DATABASE_URL")

	// Pool first, migrations second — the reverse of the original order,
	// and the swap is load-bearing now that DATABASE_URL resolves to
	// whichever node currently holds the leader role rather than to a
	// fixed container. The pool is what can ask "is there a leader yet?",
	// and pgha.WaitForWritable blocks until the answer is yes, so a
	// service that happens to start during a failover waits it out
	// instead of dying on a migration attempt against a node that is
	// still a standby. Nothing is paid for the reordering: pgha.NewPool
	// dials nothing, exactly like the pgxpool.New it replaces.
	pool, err := pgha.NewPool(context.Background(), databaseURL)
	if err != nil {
		log.Fatalf("notifications-svc: failed to create postgres pool: %v", err)
	}
	defer pool.Close()

	if err := pgha.WaitForWritable(context.Background(), pool, log.Printf); err != nil {
		log.Fatalf("notifications-svc: no writable postgres leader: %v", err)
	}

	// Retried rather than fatal on the first error: a failover landing
	// between the check above and this call is a few seconds, not a
	// reason to crash and leave the restart policy to sort it out.
	// pgha.Retry still surfaces a genuine migration failure — bad SQL, a
	// missing table, wrong credentials — immediately, so this does not
	// turn a real breakage into a two-minute silence.
	if err := pgha.Retry(context.Background(), "run migrations", log.Printf, func(context.Context) error {
		return runMigrations(databaseURL)
	}); err != nil {
		log.Fatalf("notifications-svc: failed to run migrations: %v", err)
	}

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
	dlqWriter := newKafkaWriter(kafkaBrokers, transferDLQTopic)
	defer dlqWriter.Close()

	// ctx cancels on SIGTERM/SIGINT. Every reader's FetchMessage blocks on
	// it, so cancellation is what unblocks an idle consumer and lets its
	// goroutine return. It is deliberately NOT threaded into the handling
	// of a message already fetched — see runTransferEventsConsumer's doc
	// comment for why a message in flight must finish on its own context.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	health := &consumerHealth{}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		runUserEventsConsumer(ctx, userEventsReader, pool)
	}()
	go func() {
		defer wg.Done()
		runAccountCreatedConsumer(ctx, accountEventsReader, pool)
	}()
	go func() {
		defer wg.Done()
		runTransferEventsConsumer(ctx, transferEventsReader, pool, smtpAddr, smtpFrom, dlqWriter, transferDLQTopic)
	}()

	go monitorConsumers(ctx, []namedReader{
		{userEventsTopic, userEventsReader},
		{accountEventsTopic, accountEventsReader},
		{transferEventsTopic, transferEventsReader},
	})
	go monitorKafkaHealth(ctx, kafkaBrokers, health)

	// An explicit mux, not the package-level http.DefaultServeMux, so
	// "/healthz" can be method-qualified below without any ambiguity —
	// same reasoning as transfers-svc/accounts-svc.
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		json.NewEncoder(w).Encode(map[string]string{"service": "notifications-svc"})
	})

	// Honest, unlike pkg/health.Handler (still used by gateway, which has
	// no dependency worth checking): reports 503 if Postgres is
	// unreachable OR any of the three Kafka readers is currently failing
	// to fetch, instead of always answering 200 for a process that is
	// merely still running. consumer_lag surfaces the same numbers
	// logConsumerLag writes to the log, so a quick curl answers "did it
	// fall behind or die" without needing log access.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		checkCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "application/json")

		var result int
		dbErr := pool.QueryRow(checkCtx, "SELECT 1").Scan(&result)
		kafkaOK := health.ok()

		body := map[string]any{
			"service":  "notifications-svc",
			"postgres": dbErr == nil,
			"kafka":    kafkaOK,
			"consumer_lag": map[string]int64{
				userEventsTopic:     userEventsReader.Stats().Lag,
				accountEventsTopic:  accountEventsReader.Stats().Lag,
				transferEventsTopic: transferEventsReader.Stats().Lag,
			},
		}
		if dbErr != nil || !kafkaOK {
			body["status"] = "error"
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(body)
			return
		}
		body["status"] = "ok"
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(body)
	})

	srv := &http.Server{Addr: ":" + port, Handler: tracing.Handler(mux, "notifications-svc")}
	go func() {
		<-ctx.Done()
		log.Printf("notifications-svc: shutdown signal received, draining in-flight work")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("notifications-svc: http server shutdown: %v", err)
		}
	}()

	log.Printf("notifications-svc listening on :%s", port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}

	// wg.Wait, not a timeout: each consumer goroutine returns as soon as
	// its FetchMessage unblocks on ctx with nothing in flight, or once
	// the message it already holds finishes — every retry attempt, a
	// possible DLQ publish, and the commit (see
	// runTransferEventsConsumer's doc comment). Cutting this off with a
	// deadline would recreate the exact "abandoned mid-message" problem
	// graceful shutdown exists to avoid.
	log.Printf("notifications-svc: waiting for consumers to finish their current message")
	wg.Wait()
	log.Printf("notifications-svc: shutdown complete")
}
