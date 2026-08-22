package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
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

	// MinIO connection defaults match docker-compose.yml's minio service —
	// genuinely per-environment config (a prod deploy points these at R2/S3
	// instead), unlike the avatar TTL/cleanup constants in storage.go and
	// avatar_cleanup.go, which are mechanism details, not deployment config.
	//
	// defaultMinioEndpoint (the internal docker-compose hostname) and
	// defaultMinioPublicEndpoint (the host-published port) are
	// deliberately different: this service's own Stat/Get/Put/Remove/List
	// calls use the former, but a presigned URL signed against "minio:9000"
	// resolves nowhere outside the compose network — nothing a browser (or
	// anything else on the host) could ever use. See storage.go's
	// avatarStorage doc comment for the full story; this split exists
	// because that exact mistake was caught live, not designed in upfront.
	defaultMinioEndpoint       = "minio:9000"
	defaultMinioPublicEndpoint = "localhost:9000"
	defaultMinioAccess         = "neobank"
	defaultMinioSecret         = "neobank_dev_password"
	defaultMinioBucket         = "avatars"

	// defaultAvatarUploadRateLimit/Window bound how often any one user can
	// mint a presigned upload URL — see recordAvatarUploadAttempt
	// (avatar_rate_limit.go) for why this exists at all. Looser than
	// accounts-svc's IBAN-resolve limit: a real user changing their avatar
	// a handful of times in ten minutes is unremarkable, unlike ten
	// distinct recipient lookups.
	defaultAvatarUploadRateLimit  = 5
	defaultAvatarUploadRateWindow = 10 * time.Minute
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
	minioEndpoint := os.Getenv("MINIO_ENDPOINT")
	if minioEndpoint == "" {
		minioEndpoint = defaultMinioEndpoint
	}
	minioPublicEndpoint := os.Getenv("MINIO_PUBLIC_ENDPOINT")
	if minioPublicEndpoint == "" {
		minioPublicEndpoint = defaultMinioPublicEndpoint
	}
	minioAccessKey := os.Getenv("MINIO_ACCESS_KEY")
	if minioAccessKey == "" {
		minioAccessKey = defaultMinioAccess
	}
	minioSecretKey := os.Getenv("MINIO_SECRET_KEY")
	if minioSecretKey == "" {
		minioSecretKey = defaultMinioSecret
	}
	minioBucket := os.Getenv("MINIO_BUCKET")
	if minioBucket == "" {
		minioBucket = defaultMinioBucket
	}
	minioUseSSL := os.Getenv("MINIO_USE_SSL") == "true"
	avatarUploadRateLimit := defaultAvatarUploadRateLimit
	if v := os.Getenv("AVATAR_UPLOAD_RATE_LIMIT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			log.Fatalf("auth-svc: invalid AVATAR_UPLOAD_RATE_LIMIT %q: must be a positive integer", v)
		}
		avatarUploadRateLimit = n
	}
	avatarUploadRateWindow := defaultAvatarUploadRateWindow
	if v := os.Getenv("AVATAR_UPLOAD_RATE_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("auth-svc: invalid AVATAR_UPLOAD_RATE_WINDOW %q: %v", v, err)
		}
		avatarUploadRateWindow = d
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

	// No network I/O here — same lazy-connect reasoning as the Redis and
	// Kafka clients above tolerate a not-yet-ready dependency at startup.
	avatarStore, err := newAvatarStorage(minioEndpoint, minioPublicEndpoint, minioAccessKey, minioSecretKey, minioBucket, minioUseSSL)
	if err != nil {
		log.Fatalf("auth-svc: failed to create minio client: %v", err)
	}

	go outbox.RunRelay(context.Background(), pool, authOutboxTable, kafkaWriter, outboxRelayInterval, outbox.DefaultBatchSize, "auth-svc")
	go outbox.RunCleanupWorker(context.Background(), pool, authOutboxTable, outboxRetention, outbox.DefaultCleanupInterval, "auth-svc")
	go runAvatarUploadAttemptsCleanupWorker(context.Background(), pool)
	go runAvatarCleanupWorker(context.Background(), avatarStore)

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
	// Method-qualified (unlike the bare paths above): GET and PATCH are
	// genuinely different operations here, and mixing a method-qualified
	// pattern into an otherwise bare-path mux is supported — accounts-svc
	// already does the same thing on its own mux.
	http.HandleFunc("GET /profile", getProfileHandler(pool, avatarStore))
	http.HandleFunc("PATCH /profile", updateProfileHandler(pool, avatarStore))
	http.HandleFunc("POST /profile/avatar/upload-url", uploadAvatarURLHandler(pool, avatarStore, avatarUploadRateLimit, avatarUploadRateWindow))
	http.HandleFunc("POST /profile/avatar/confirm", confirmAvatarHandler(pool, avatarStore))

	log.Printf("auth-svc listening on :%s", port)
	// http.DefaultServeMux named explicitly rather than passing nil:
	// the handler has to be wrapped, and there is nothing to wrap when
	// the argument is nil.
	if err := http.ListenAndServe(":"+port, tracing.Handler(http.DefaultServeMux, "auth-svc")); err != nil {
		log.Fatal(err)
	}
}
