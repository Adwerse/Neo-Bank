package main

import (
	"context"
	"log"
	"net"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

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

	if err := runMigrations(databaseURL); err != nil {
		log.Fatalf("ledger-svc: failed to run migrations: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		log.Fatalf("ledger-svc: failed to create postgres pool: %v", err)
	}
	defer pool.Close()

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
