package main

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
)

type route struct {
	prefix string
	addr   string
}

func routes() []route {
	return []route{
		{"/auth", envOr("AUTH_SVC_ADDR", "auth-svc:8081")},
		{"/accounts", envOr("ACCOUNTS_SVC_ADDR", "accounts-svc:8082")},
		{"/transfers", envOr("TRANSFERS_SVC_ADDR", "transfers-svc:8084")},
		{"/fraud", envOr("FRAUD_SVC_ADDR", "fraud-svc:8085")},
		{"/notifications", envOr("NOTIFICATIONS_SVC_ADDR", "notifications-svc:8086")},
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newProxy(prefix, addr string) http.Handler {
	target := &url.URL{Scheme: "http", Host: addr}
	return http.StripPrefix(prefix, httputil.NewSingleHostReverseProxy(target))
}

// newNoStripProxy forwards the request path to addr completely unchanged —
// for the handful of transfers-svc routes (POST /deposits, GET
// /deposits/{id}, POST /webhooks/stripe) that live on transfers-svc's own
// mux as siblings of "/", not nested under an internal "/transfers"
// prefix the way newProxy's callers expect. Stripping a prefix like
// "/deposits" down to "/" here would silently misroute a deposit request
// to transfers-svc's transfer handler instead — see the callers in
// main.go for the full reasoning.
func newNoStripProxy(addr string) http.Handler {
	target := &url.URL{Scheme: "http", Host: addr}
	return httputil.NewSingleHostReverseProxy(target)
}
