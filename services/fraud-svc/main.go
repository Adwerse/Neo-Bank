package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"neobank/pkg/pgha"
	fraudv1 "neobank/proto/gen/go/fraud/v1"
)

const (
	defaultPort     = "8085"
	defaultGRPCPort = "9085"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}
	grpcPort := os.Getenv("GRPC_PORT")
	if grpcPort == "" {
		grpcPort = defaultGRPCPort
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
		log.Fatalf("fraud-svc: failed to create postgres pool: %v", err)
	}
	defer pool.Close()

	if err := pgha.WaitForWritable(context.Background(), pool, log.Printf); err != nil {
		log.Fatalf("fraud-svc: no writable postgres leader: %v", err)
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
		log.Fatalf("fraud-svc: failed to run migrations: %v", err)
	}

	lis, err := net.Listen("tcp", ":"+grpcPort)
	if err != nil {
		log.Fatalf("fraud-svc: failed to listen on :%s (gRPC): %v", grpcPort, err)
	}
	grpcServer := grpc.NewServer()
	fraudv1.RegisterFraudServiceServer(grpcServer, &fraudServer{pool: pool})

	grpcHealthServer := health.NewServer()
	grpcHealthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(grpcServer, grpcHealthServer)

	reflection.Register(grpcServer)

	go func() {
		log.Printf("fraud-svc listening on :%s (gRPC)", grpcPort)
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatal(err)
		}
	}()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		json.NewEncoder(w).Encode(map[string]string{"service": "fraud-svc"})
	})

	http.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		w.Header().Set("Content-Type", "application/json")
		var result int
		if err := pool.QueryRow(ctx, "SELECT 1").Scan(&result); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "service": "fraud-svc"})
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "service": "fraud-svc"})
	})

	log.Printf("fraud-svc listening on :%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}
