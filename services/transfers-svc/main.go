package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/stripe/stripe-go/v86"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"neobank/pkg/outbox"
	"neobank/pkg/pgha"
	"neobank/pkg/tracing"
	accountsv1 "neobank/proto/gen/go/accounts/v1"
	fraudv1 "neobank/proto/gen/go/fraud/v1"
	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

const (
	defaultPort                       = "8084"
	defaultAccountsAddr               = "accounts-svc:9082"
	defaultFraudAddr                  = "fraud-svc:9085"
	defaultLedgerAddr                 = "ledger-svc:8083"
	defaultReconcileStaleAfter        = 2 * time.Minute
	defaultDepositReconcileStaleAfter = 2 * time.Minute
	defaultKafkaBrokers               = "kafka:9092"
	defaultTransferEventsTopic        = "transfer.events"
	defaultOutboxRetention            = 7 * 24 * time.Hour
	outboxRelayInterval               = 1 * time.Second
)

// stripeClient is the Stripe SDK client, constructed once at startup from
// STRIPE_SECRET_KEY (see main()). It is package-level rather than a
// main()-local variable because no HTTP handler in transfers-svc consumes
// it yet — PaymentIntent creation and webhook handling are future work
// (see README, "Stripe-funded deposits"). Go only rejects unused
// *local* variables, so this compiles cleanly without a fabricated
// consumer, while the client is still fully constructed and validated
// (secret key present) before the service starts serving traffic.
var stripeClient *stripe.Client

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}
	accountsAddr := os.Getenv("ACCOUNTS_GRPC_ADDR")
	if accountsAddr == "" {
		accountsAddr = defaultAccountsAddr
	}
	fraudAddr := os.Getenv("FRAUD_GRPC_ADDR")
	if fraudAddr == "" {
		fraudAddr = defaultFraudAddr
	}
	ledgerAddr := os.Getenv("LEDGER_GRPC_ADDR")
	if ledgerAddr == "" {
		ledgerAddr = defaultLedgerAddr
	}
	reconcileStaleAfter := defaultReconcileStaleAfter
	if v := os.Getenv("RECONCILE_STALE_AFTER"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("transfers-svc: invalid RECONCILE_STALE_AFTER %q: %v", v, err)
		}
		reconcileStaleAfter = d
	}
	depositReconcileStaleAfter := defaultDepositReconcileStaleAfter
	if v := os.Getenv("DEPOSIT_RECONCILE_STALE_AFTER"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("transfers-svc: invalid DEPOSIT_RECONCILE_STALE_AFTER %q: %v", v, err)
		}
		depositReconcileStaleAfter = d
	}
	outboxRetention := defaultOutboxRetention
	if v := os.Getenv("OUTBOX_RETENTION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("transfers-svc: invalid OUTBOX_RETENTION %q: %v", v, err)
		}
		outboxRetention = d
	}
	kafkaBrokers := os.Getenv("KAFKA_BROKERS")
	if kafkaBrokers == "" {
		kafkaBrokers = defaultKafkaBrokers
	}
	transferEventsTopic := os.Getenv("KAFKA_TRANSFER_EVENTS_TOPIC")
	if transferEventsTopic == "" {
		transferEventsTopic = defaultTransferEventsTopic
	}
	stripeSecretKey := os.Getenv("STRIPE_SECRET_KEY")
	if stripeSecretKey == "" {
		log.Fatal("transfers-svc: STRIPE_SECRET_KEY environment variable is required")
	}
	// stripe.NewClient does no network I/O — same lazy-init philosophy as
	// the grpc.NewClient calls below.
	stripeClient = stripe.NewClient(stripeSecretKey)
	// Separate from STRIPE_SECRET_KEY by design: this signs webhook
	// deliveries, not API requests, and is the one thing standing between
	// stripeWebhookHandler and anyone on the internet claiming a payment
	// succeeded (see README, "Local webhook development" for how to obtain a
	// local value via the Stripe CLI).
	stripeWebhookSecret := os.Getenv("STRIPE_WEBHOOK_SECRET")
	if stripeWebhookSecret == "" {
		log.Fatal("transfers-svc: STRIPE_WEBHOOK_SECRET environment variable is required")
	}

	// Tracing is set up before anything that could produce a span. A
	// failure is logged rather than fatal: observability going down must
	// not take the service with it — the global provider simply stays the
	// API's no-op and everything else behaves identically.
	shutdownTracing, err := tracing.Init(context.Background(), "transfers-svc")
	if err != nil {
		log.Printf("transfers-svc: tracing disabled: %v", err)
		shutdownTracing = func(context.Context) error { return nil }
	}
	defer func() {
		if err := shutdownTracing(context.Background()); err != nil {
			log.Printf("transfers-svc: tracing shutdown: %v", err)
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
		log.Fatalf("transfers-svc: failed to create postgres pool: %v", err)
	}
	defer pool.Close()

	if err := pgha.WaitForWritable(context.Background(), pool, log.Printf); err != nil {
		log.Fatalf("transfers-svc: no writable postgres leader: %v", err)
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
		log.Fatalf("transfers-svc: failed to run migrations: %v", err)
	}

	// Falls back to the leader when unset (local `go test`, and any
	// deployment without a standby to read from) rather than failing to
	// start — reading from the leader is always correct, just not the
	// point of having standbys. What's NOT a fallback is which queries
	// are allowed to use this pool at all once it does point at a real
	// standby: see listTransfersHandler's doc comment and README.
	//
	// Deliberately NOT wrapped in WaitForWritable, unlike the pool above:
	// this one is supposed to land on a node in recovery, so "is it
	// writable yet" is the wrong question to block startup on. If no
	// standby is available the pool simply fails its queries and
	// listTransfersHandler returns an error for a history page — a
	// degraded read, not a service that refuses to boot.
	readReplicaURL := os.Getenv("DATABASE_URL_REPLICA")
	if readReplicaURL == "" {
		readReplicaURL = databaseURL
	}
	readPool, err := pgha.NewPool(context.Background(), readReplicaURL)
	if err != nil {
		log.Fatalf("transfers-svc: failed to create postgres read-replica pool: %v", err)
	}
	defer readPool.Close()

	// grpc.NewClient is lazy: it does not block on a live accounts-svc here,
	// it dials on the first RPC and reconnects on its own — matching how
	// accounts-svc's own ledger client tolerates a not-yet-ready dependency
	// at startup. accounts-svc speaks plaintext gRPC inside the cluster (no
	// TLS), same as its own server setup.
	accountsConn, err := grpc.NewClient(accountsAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), tracing.GRPCClientOption())
	if err != nil {
		log.Fatalf("transfers-svc: failed to create accounts gRPC client for %s: %v", accountsAddr, err)
	}
	defer accountsConn.Close()
	accountsClient := accountsv1.NewAccountsServiceClient(accountsConn)

	// Same lazy-dial reasoning as accountsConn above.
	fraudConn, err := grpc.NewClient(fraudAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), tracing.GRPCClientOption())
	if err != nil {
		log.Fatalf("transfers-svc: failed to create fraud gRPC client for %s: %v", fraudAddr, err)
	}
	defer fraudConn.Close()
	fraudClient := fraudv1.NewFraudServiceClient(fraudConn)

	// Same lazy-dial reasoning as accountsConn above.
	ledgerConn, err := grpc.NewClient(ledgerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()), tracing.GRPCClientOption())
	if err != nil {
		log.Fatalf("transfers-svc: failed to create ledger gRPC client for %s: %v", ledgerAddr, err)
	}
	defer ledgerConn.Close()
	ledgerClient := ledgerv1.NewLedgerServiceClient(ledgerConn)

	kafkaWriter := newKafkaWriter(kafkaBrokers, transferEventsTopic)
	defer kafkaWriter.Close()

	go runReconciliationWorker(context.Background(), pool, ledgerClient, reconcileStaleAfter, stripeClient.V1PaymentIntents, depositReconcileStaleAfter)
	go outbox.RunRelay(context.Background(), pool, outboxTable, kafkaWriter, outboxRelayInterval, outbox.DefaultBatchSize, "transfers-svc")
	go outbox.RunCleanupWorker(context.Background(), pool, outboxTable, outboxRetention, outbox.DefaultCleanupInterval, "transfers-svc")

	// An explicit mux, not the package-level http.DefaultServeMux: importing
	// google.golang.org/grpc transitively pulls in golang.org/x/net/trace,
	// whose init() registers "/debug/requests" on DefaultServeMux. That
	// bare, any-method pattern is genuinely ambiguous against "POST /"
	// below (neither is strictly more specific — one wins on method, the
	// other on path), which panics at registration time. gateway/main.go
	// already sidesteps this the same way, with its own mux.
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		json.NewEncoder(w).Encode(map[string]string{"service": "transfers-svc"})
	})

	// Method-qualified (not bare "/healthz") so it doesn't create an
	// unresolvable ambiguity with "POST /" below — same reasoning as
	// accounts-svc's "GET /healthz".
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		w.Header().Set("Content-Type", "application/json")
		var result int
		if err := pool.QueryRow(ctx, "SELECT 1").Scan(&result); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "service": "transfers-svc"})
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "transfers-svc"})
	})

	mux.HandleFunc("POST /", createTransferHandler(pool, accountsClient, fraudClient, ledgerClient))
	mux.HandleFunc("GET /", listTransfersHandler(pool, readPool, accountsClient))
	mux.HandleFunc("POST /deposits", createDepositHandler(pool, accountsClient, stripeClient.V1PaymentIntents))
	mux.HandleFunc("GET /deposits/{id}", getDepositHandler(pool, accountsClient))
	mux.HandleFunc("POST /webhooks/stripe", stripeWebhookHandler(pool, ledgerClient, stripeWebhookSecret))
	mux.HandleFunc("POST /withdrawals", createWithdrawalHandler(pool, accountsClient, ledgerClient))

	log.Printf("transfers-svc listening on :%s", port)
	if err := http.ListenAndServe(":"+port, tracing.Handler(mux, "transfers-svc")); err != nil {
		log.Fatal(err)
	}
}
