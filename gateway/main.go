package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"

	"neobank/pkg/health"
)

const defaultPort = "8080"

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}
	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		log.Fatal("gateway: JWT_SECRET environment variable is required")
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		json.NewEncoder(w).Encode(map[string]string{"service": "gateway"})
	})
	mux.HandleFunc("/healthz", health.Handler("gateway"))

	for _, rt := range routes() {
		mux.Handle(rt.prefix+"/", newProxy(rt.prefix, rt.addr))
	}

	log.Printf("gateway listening on :%s", port)
	if err := http.ListenAndServe(":"+port, jwtMiddleware(mux, jwtSecret)); err != nil {
		log.Fatal(err)
	}
}
