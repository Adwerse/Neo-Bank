package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

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

	if err := runMigrations(databaseURL); err != nil {
		log.Fatalf("fraud-svc: failed to run migrations: %v", err)
	}

	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		log.Fatalf("fraud-svc: failed to create postgres pool: %v", err)
	}
	defer pool.Close()

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
