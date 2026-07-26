package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	accountsv1 "neobank/proto/gen/go/accounts/v1"
	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

const (
	defaultPort         = "8084"
	defaultAccountsAddr = "accounts-svc:9082"
	defaultLedgerAddr   = "ledger-svc:8083"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}
	accountsAddr := os.Getenv("ACCOUNTS_GRPC_ADDR")
	if accountsAddr == "" {
		accountsAddr = defaultAccountsAddr
	}
	ledgerAddr := os.Getenv("LEDGER_GRPC_ADDR")
	if ledgerAddr == "" {
		ledgerAddr = defaultLedgerAddr
	}
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
	ledgerConn, err := grpc.NewClient(ledgerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("transfers-svc: failed to create ledger gRPC client for %s: %v", ledgerAddr, err)
	}
	defer ledgerConn.Close()
	ledgerClient := ledgerv1.NewLedgerServiceClient(ledgerConn)

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

	mux.HandleFunc("POST /", createTransferHandler(pool, accountsClient, ledgerClient))

	log.Printf("transfers-svc listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}
