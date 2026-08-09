package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"neobank/pkg/outbox"
	"neobank/pkg/pgha"
	"neobank/pkg/tracing"
)

const (
	defaultPort            = "8081"
	defaultSMTPAddr        = "mailpit:1025"
	defaultSMTPFrom        = "noreply@neobank.local"
	defaultRedisAddr       = "redis:6379"
	defaultKafkaBrokers    = "kafka:9092"
	defaultKafkaTopic      = "user.events"
	defaultOutboxRetention = 7 * 24 * time.Hour
	outboxRelayInterval    = 1 * time.Second
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
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = defaultRedisAddr
	}
	kafkaBrokers := os.Getenv("KAFKA_BROKERS")
	if kafkaBrokers == "" {
		kafkaBrokers = defaultKafkaBrokers
	}
	kafkaTopic := os.Getenv("KAFKA_TOPIC")
	if kafkaTopic == "" {
		kafkaTopic = defaultKafkaTopic
	}
	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		log.Fatal("auth-svc: JWT_SECRET environment variable is required")
	}
	outboxRetention := defaultOutboxRetention
	if v := os.Getenv("OUTBOX_RETENTION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("auth-svc: invalid OUTBOX_RETENTION %q: %v", v, err)
		}
		outboxRetention = d
	}
	// Tracing is set up before anything that could produce a span. A
	// failure is logged rather than fatal: observability going down must
	// not take the service with it — the global provider simply stays the
	// API's no-op and everything else behaves identically.
	shutdownTracing, err := tracing.Init(context.Background(), "auth-svc")
	if err != nil {
		log.Printf("auth-svc: tracing disabled: %v", err)
		shutdownTracing = func(context.Context) error { return nil }
	}
	defer func() {
		if err := shutdownTracing(context.Background()); err != nil {
			log.Printf("auth-svc: tracing shutdown: %v", err)
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
		log.Fatalf("auth-svc: failed to create postgres pool: %v", err)
	}
	defer pool.Close()

	if err := pgha.WaitForWritable(context.Background(), pool, log.Printf); err != nil {
		log.Fatalf("auth-svc: no writable postgres leader: %v", err)
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
		log.Fatalf("auth-svc: failed to run migrations: %v", err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer rdb.Close()

	kafkaWriter := newKafkaWriter(kafkaBrokers, kafkaTopic)
	defer kafkaWriter.Close()

	go outbox.RunRelay(context.Background(), pool, authOutboxTable, kafkaWriter, outboxRelayInterval, outbox.DefaultBatchSize, "auth-svc")
	go outbox.RunCleanupWorker(context.Background(), pool, authOutboxTable, outboxRetention, outbox.DefaultCleanupInterval, "auth-svc")

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
	http.HandleFunc("/verify-email", verifyEmailHandler(pool))
	http.HandleFunc("/resend-verification", resendVerificationHandler(pool, rdb, smtpAddr, smtpFrom))
	http.HandleFunc("/login", loginHandler(pool, rdb, jwtSecret))
	http.HandleFunc("/refresh", refreshHandler(pool, rdb, jwtSecret))
	http.HandleFunc("/logout", logoutHandler(rdb))
	http.HandleFunc("/forgot-password", forgotPasswordHandler(pool, rdb, smtpAddr, smtpFrom))
	http.HandleFunc("/reset-password", resetPasswordHandler(pool, rdb))

	log.Printf("auth-svc listening on :%s", port)
	// http.DefaultServeMux named explicitly rather than passing nil:
	// the handler has to be wrapped, and there is nothing to wrap when
	// the argument is nil.
	if err := http.ListenAndServe(":"+port, tracing.Handler(http.DefaultServeMux, "auth-svc")); err != nil {
		log.Fatal(err)
	}
}
