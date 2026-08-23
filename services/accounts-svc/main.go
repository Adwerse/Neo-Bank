package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"neobank/pkg/outbox"
	"neobank/pkg/pgha"
	"neobank/pkg/tracing"
	accountsv1 "neobank/proto/gen/go/accounts/v1"
	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

const (
	defaultPort               = "8082"
	defaultGRPCPort           = "9082"
	defaultKafkaBrokers       = "kafka:9092"
	defaultKafkaTopic         = "user.events"
	defaultAccountEventsTopic = "account.events"
	kafkaConsumerGroup        = "accounts-svc"
	defaultLedgerAddr         = "ledger-svc:8083"
	defaultOutboxRetention    = 7 * 24 * time.Hour
	outboxRelayInterval       = 1 * time.Second

	// defaultBankCode is a deliberately fictitious 4-letter institution
	// code — not a near-miss of any real registered Irish bank (AIBK,
	// BOFI, ULSB, PTSB, KBIE, NIRE, ...). See README's "Честные
	// ограничения" for why generated IBANs must not point at a real
	// organization.
	defaultBankCode = "ZZZZ"

	// defaultResolveRateLimit/Window bound ResolveAccountByIban's per-user
	// attempt rate (grpc_server.go, rate_limit.go): generous enough for a
	// real user retrying a mistyped IBAN a few times, tight enough to
	// slow down scripted enumeration of which IBANs exist.
	defaultResolveRateLimit  = 10
	defaultResolveRateWindow = 5 * time.Minute
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
	accountEventsTopic := os.Getenv("KAFKA_ACCOUNT_EVENTS_TOPIC")
	if accountEventsTopic == "" {
		accountEventsTopic = defaultAccountEventsTopic
	}
	ledgerAddr := os.Getenv("LEDGER_GRPC_ADDR")
	if ledgerAddr == "" {
		ledgerAddr = defaultLedgerAddr
	}
	bankCode := os.Getenv("BANK_CODE")
	if bankCode == "" {
		bankCode = defaultBankCode
	}
	resolveRateLimit := defaultResolveRateLimit
	if v := os.Getenv("IBAN_RESOLVE_RATE_LIMIT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			log.Fatalf("accounts-svc: invalid IBAN_RESOLVE_RATE_LIMIT %q: must be a positive integer", v)
		}
		resolveRateLimit = n
	}
	resolveRateWindow := defaultResolveRateWindow
	if v := os.Getenv("IBAN_RESOLVE_RATE_WINDOW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("accounts-svc: invalid IBAN_RESOLVE_RATE_WINDOW %q: %v", v, err)
		}
		resolveRateWindow = d
	}
	outboxRetention := defaultOutboxRetention
	if v := os.Getenv("OUTBOX_RETENTION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("accounts-svc: invalid OUTBOX_RETENTION %q: %v", v, err)
		}
		outboxRetention = d
	}
	// Tracing is set up before anything that could produce a span. A
	// failure is logged rather than fatal: observability going down must
	// not take the service with it — the global provider simply stays the
	// API's no-op and everything else behaves identically.
	shutdownTracing, err := tracing.Init(context.Background(), "accounts-svc")
	if err != nil {
		log.Printf("accounts-svc: tracing disabled: %v", err)
		shutdownTracing = func(context.Context) error { return nil }
	}
	defer func() {
		if err := shutdownTracing(context.Background()); err != nil {
			log.Printf("accounts-svc: tracing shutdown: %v", err)
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
		log.Fatalf("accounts-svc: failed to create postgres pool: %v", err)
	}
	defer pool.Close()

	if err := pgha.WaitForWritable(context.Background(), pool, log.Printf); err != nil {
		log.Fatalf("accounts-svc: no writable postgres leader: %v", err)
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
		log.Fatalf("accounts-svc: failed to run migrations: %v", err)
	}

	// Backfill runs synchronously, before any listener starts accepting
	// requests: every accounts row must have a non-NULL iban by the time
	// getAccountByUserID/etc. can possibly be called, since their Scan
	// targets a plain string field, not a nullable one.
	if err := pgha.Retry(context.Background(), "backfill ibans", log.Printf, func(context.Context) error {
		return backfillIBANs(context.Background(), pool, bankCode)
	}); err != nil {
		log.Fatalf("accounts-svc: failed to backfill ibans: %v", err)
	}

	// grpc.NewClient is lazy: it does not block on a live ledger-svc here, it
	// dials on the first RPC and reconnects on its own — matching how the
	// Postgres and Kafka clients above tolerate a not-yet-ready dependency at
	// startup. ledger-svc speaks plaintext gRPC inside the cluster (no TLS),
	// same as its own server setup.
	ledgerConn, err := grpc.NewClient(ledgerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), tracing.GRPCClientOption())
	if err != nil {
		log.Fatalf("accounts-svc: failed to create ledger gRPC client for %s: %v", ledgerAddr, err)
	}
	defer ledgerConn.Close()
	ledgerClient := ledgerv1.NewLedgerServiceClient(ledgerConn)

	kafkaReader := newKafkaReader(kafkaBrokers, kafkaTopic, kafkaConsumerGroup)
	defer kafkaReader.Close()

	kafkaWriter := newKafkaWriter(kafkaBrokers, accountEventsTopic)
	defer kafkaWriter.Close()

	go runUserActivatedConsumer(context.Background(), kafkaReader, pool, ledgerClient, bankCode)
	go outbox.RunRelay(context.Background(), pool, accountsOutboxTable, kafkaWriter, outboxRelayInterval, outbox.DefaultBatchSize, "accounts-svc")
	go outbox.RunCleanupWorker(context.Background(), pool, accountsOutboxTable, outboxRetention, outbox.DefaultCleanupInterval, "accounts-svc")
	go runResolveAttemptsCleanupWorker(context.Background(), pool)

	grpcPort := os.Getenv("GRPC_PORT")
	if grpcPort == "" {
		grpcPort = defaultGRPCPort
	}
	lis, err := net.Listen("tcp", ":"+grpcPort)
	if err != nil {
		log.Fatalf("accounts-svc: failed to listen on :%s (gRPC): %v", grpcPort, err)
	}
	// StatsHandler makes every inbound RPC a server span that continues
	// the caller's trace (the context travels in gRPC metadata), which is
	// what puts this service under transfers-svc in the same trace rather
	// than in a trace of its own.
	grpcServer := grpc.NewServer(tracing.GRPCServerOption())
	accountsv1.RegisterAccountsServiceServer(grpcServer, &accountsServer{
		pool:              pool,
		bankCode:          bankCode,
		resolveRateLimit:  resolveRateLimit,
		resolveRateWindow: resolveRateWindow,
	})

	grpcHealthServer := health.NewServer()
	grpcHealthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcServer, grpcHealthServer)

	reflection.Register(grpcServer)

	go func() {
		log.Printf("accounts-svc listening on :%s (gRPC)", grpcPort)
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatal(err)
		}
	}()

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
	http.HandleFunc("GET /me", meAccountHandler(pool, ledgerClient))
	http.HandleFunc("GET /me/balance-history", balanceHistoryHandler(pool, ledgerClient))
	http.HandleFunc("GET /{id}", getAccountHandler(pool))
	http.HandleFunc("PATCH /{id}/status", updateAccountStatusHandler(pool))

	log.Printf("accounts-svc listening on :%s", port)
	// http.DefaultServeMux named explicitly rather than passing nil:
	// the handler has to be wrapped, and there is nothing to wrap when
	// the argument is nil.
	if err := http.ListenAndServe(":"+port, tracing.Handler(http.DefaultServeMux, "accounts-svc")); err != nil {
		log.Fatal(err)
	}
}
