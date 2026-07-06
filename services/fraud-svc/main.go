package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"

	"neobank/pkg/health"
)

const defaultPort = "8085"

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		json.NewEncoder(w).Encode(map[string]string{"service": "fraud-svc"})
	})
	http.HandleFunc("/healthz", health.Handler("fraud-svc"))

	log.Printf("fraud-svc listening on :%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}
