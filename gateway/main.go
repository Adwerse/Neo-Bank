package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"neobank/pkg/health"
)

const (
	defaultPort = "8080"

	// shutdownTimeout bounds how long SIGTERM/SIGINT gives http.Server to
	// stop accepting new connections. It does NOT bound open WebSocket
	// connections — those are hijacked TCP conns that Shutdown doesn't
	// track at all, which is why ws.go's handleWS independently watches
	// the same shutdown context and sends its own close frame (see
	// newHandler below).
	shutdownTimeout = 10 * time.Second
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}
	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		log.Fatal("gateway: JWT_SECRET environment variable is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: ":" + port, Handler: newHandler(ctx, jwtSecret)}

	go func() {
		<-ctx.Done()
		log.Printf("gateway: shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("gateway: http server shutdown: %v", err)
		}
	}()

	log.Printf("gateway listening on :%s", port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// newHandler builds the full gateway handler (routing + JWT middleware),
// separated from main() so gateway_test.go can exercise real routing
// behavior (redirects, prefix stripping, the public webhook allowlist)
// against an httptest.Server without a live JWT_SECRET/port bind. ctx is
// the process's shutdown context (canceled on SIGTERM/SIGINT) — GET /ws
// needs it directly since http.Server.Shutdown never sees WebSocket
// connections (see shutdownTimeout's comment).
func newHandler(ctx context.Context, jwtSecret string) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		json.NewEncoder(w).Encode(map[string]string{"service": "gateway"})
	})
	mux.HandleFunc("/healthz", health.Handler("gateway"))

	ws := newWSServer(ctx, jwtSecret)
	mux.HandleFunc("GET /ws", ws.handleWS)

	for _, rt := range routes() {
		mux.Handle(rt.prefix+"/", newProxy(rt.prefix, rt.addr))
	}

	// POST /deposits, GET /deposits/{id}, and POST /webhooks/stripe all
	// live on transfers-svc's own mux as top-level paths (siblings of "/",
	// see services/transfers-svc/main.go) rather than nested under an
	// internal prefix — so unlike every route above, forwarding must NOT
	// strip anything.
	//
	// "/deposits" is registered twice (exact, then subtree) rather than
	// just "/deposits/": ServeMux auto-redirects a bare "/deposits" request
	// to "/deposits/" for a subtree-only registration, and a 301 on a POST
	// is dangerous — fetch() demotes the retried request to GET, silently
	// dropping the body (see frontend/src/features/transfers/api.ts's own
	// comment on the identical concern for POST /transfers/). The exact
	// registration overrides that redirect for the literal "/deposits"
	// path (matching transfers-svc's existing "POST /deposits" exactly, no
	// changes needed there); the subtree registration handles
	// "/deposits/{id}" (which is never the bare root, so never at redirect
	// risk in the first place).
	//
	// "/webhooks/stripe" only ever needs the exact registration — there is
	// no sub-path under it, and Stripe must never encounter a redirect on
	// its webhook POST.
	transfersAddr := envOr("TRANSFERS_SVC_ADDR", "transfers-svc:8084")
	noStripProxy := newNoStripProxy(transfersAddr)
	mux.Handle("/deposits", noStripProxy)
	mux.Handle("/deposits/", noStripProxy)
	mux.Handle("/webhooks/stripe", noStripProxy)

	return jwtMiddleware(mux, jwtSecret)
}
