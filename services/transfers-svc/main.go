package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v86"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"neobank/pkg/outbox"
	accountsv1 "neobank/proto/gen/go/accounts/v1"
	fraudv1 "neobank/proto/gen/go/fraud/v1"
	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

const (
	defaultPort                = "8084"
	defaultAccountsAddr        = "accounts-svc:9082"
	defaultFraudAddr           = "fraud-svc:9085"
	defaultLedgerAddr          = "ledger-svc:8083"
	defaultReconcileStaleAfter = 2 * time.Minute
	defaultKafkaBrokers        = "kafka:9092"
	defaultTransferEventsTopic = "transfer.events"
	defaultOutboxRetention     = 7 * 24 * time.Hour
	outboxRelayInterval        = 1 * time.Second
)

// stripeClient is the Stripe SDK client, constructed once at startup from
// STRIPE_SECRET_KEY (see main()). It is package-level rather than a
// main()-local variable because no HTTP handler in transfers-svc consumes
// it yet — PaymentIntent creation and webhook handling are future work
// (see README, "Stripe-фондированные депозиты"). Go only rejects unused
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

	databaseURL := os.Getenv("DATABASE_URL")

	if err := runMigrations(databaseURL); err != nil {
		log.Fatalf("transfers-svc: failed to run migrations: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		log.Fatalf("transfers-svc: failed to create postgres pool: %v", err)
	}
	defer pool.Close()

	// grpc.NewClient is lazy: it does not block on a live accounts-svc here,
	// it dials on the first RPC and reconnects on its own — matching how
	// accounts-svc's own ledger client tolerates a not-yet-ready dependency
	// at startup. accounts-svc speaks plaintext gRPC inside the cluster (no
	// TLS), same as its own server setup.
	accountsConn, err := grpc.NewClient(accountsAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("transfers-svc: failed to create accounts gRPC client for %s: %v", accountsAddr, err)
	}
	defer accountsConn.Close()
	accountsClient := accountsv1.NewAccountsServiceClient(accountsConn)

	// Same lazy-dial reasoning as accountsConn above.
	fraudConn, err := grpc.NewClient(fraudAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("transfers-svc: failed to create fraud gRPC client for %s: %v", fraudAddr, err)
	}
	defer fraudConn.Close()
	fraudClient := fraudv1.NewFraudServiceClient(fraudConn)

	// Same lazy-dial reasoning as accountsConn above.
	ledgerConn, err := grpc.NewClient(ledgerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("transfers-svc: failed to create ledger gRPC client for %s: %v", ledgerAddr, err)
	}
	defer ledgerConn.Close()
	ledgerClient := ledgerv1.NewLedgerServiceClient(ledgerConn)

	kafkaWriter := newKafkaWriter(kafkaBrokers, transferEventsTopic)
	defer kafkaWriter.Close()

	go runReconciliationWorker(context.Background(), pool, ledgerClient, reconcileStaleAfter)
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
	mux.HandleFunc("GET /", listTransfersHandler(pool, accountsClient))

	log.Printf("transfers-svc listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}
