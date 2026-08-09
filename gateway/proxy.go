package main

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"

	"neobank/pkg/tracing"
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

// tracedReverseProxy builds a reverse proxy whose outbound requests carry
// the current trace.
//
// The Transport line is the whole reason this helper exists, and it is the
// single most load-bearing line in the gateway for tracing. The gateway is
// the ROOT of every trace: an inbound request from the browser has no
// traceparent header at all, so there is nothing for the proxy to "not
// strip" — httputil.ReverseProxy copies inbound headers faithfully and
// would still forward nothing, because the header does not exist yet. It
// has to be INJECTED from the server span this process just created, and
// tracing.Transport is what does that on the way out.
//
// Get this wrong and the failure is quiet and specific: Jaeger shows a
// one-span gateway trace, plus a completely separate root trace for
// transfers-svc and another for fraud-svc — three disconnected traces per
// request that each look individually fine.
func tracedReverseProxy(target *url.URL) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = tracing.Transport(nil)
	return proxy
}

func newProxy(prefix, addr string) http.Handler {
	target := &url.URL{Scheme: "http", Host: addr}
	return http.StripPrefix(prefix, tracedReverseProxy(target))
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
	return tracedReverseProxy(target)
}
