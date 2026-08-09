package main

import (
	"context"
	"log"
	"net"
	"os"

	"neobank/pkg/pgha"
	ledgerv1 "neobank/proto/gen/go/ledger/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

const defaultPort = "8083"

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}
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
		log.Fatalf("ledger-svc: failed to create postgres pool: %v", err)
	}
	defer pool.Close()

	if err := pgha.WaitForWritable(context.Background(), pool, log.Printf); err != nil {
		log.Fatalf("ledger-svc: no writable postgres leader: %v", err)
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
		log.Fatalf("ledger-svc: failed to run migrations: %v", err)
	}

	lis, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatalf("ledger-svc: failed to listen on :%s: %v", port, err)
	}

	grpcServer := grpc.NewServer()
	ledgerv1.RegisterLedgerServiceServer(grpcServer, &ledgerServer{pool: pool})

	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcServer, healthServer)

	// Reflection lets grpcurl (and similar tools) list/describe/call RPCs
	// without needing the .proto files on hand — this is an internal-only
	// service (no gateway route, no client-facing use), so the usual
	// tradeoff against exposing reflection publicly doesn't apply here.
	reflection.Register(grpcServer)

	log.Printf("ledger-svc listening on :%s (gRPC)", port)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatal(err)
	}
}
