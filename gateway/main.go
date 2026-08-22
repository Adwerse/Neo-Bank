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
	"neobank/pkg/tracing"
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

	// The gateway is the root of every trace: it is the only service the
	// browser talks to, so the span it creates per request is the one every
	// downstream span in transfers-svc, fraud-svc and ledger-svc descends
	// from.
	//
	// A failure here is logged, not fatal. Tracing is diagnostics — a bank
	// that refuses to serve requests because its observability backend is
	// unreachable has traded a real outage for an imaginary one. On error,
	// the global provider stays the API's no-op and everything else works
	// exactly as before, just without traces.
	shutdownTracing, err := tracing.Init(ctx, "gateway")
	if err != nil {
		log.Printf("gateway: tracing disabled: %v", err)
		shutdownTracing = func(context.Context) error { return nil }
	}
	defer func() {
		if err := shutdownTracing(context.Background()); err != nil {
			log.Printf("gateway: tracing shutdown: %v", err)
		}
	}()

	ws := newWSServer(ctx, jwtSecret)

	accountCache := newAccountCache(envDurationOr("ACCOUNT_CACHE_TTL", defaultAccountCacheTTL), envOr("ACCOUNTS_SVC_ADDR", "accounts-svc:8082"))
	startKafkaConsumers(ctx, ws.registry, accountCache)

	srv := &http.Server{Addr: ":" + port, Handler: newHandler(jwtSecret, ws)}

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
// against an httptest.Server without a live JWT_SECRET/port bind. ws is
// built by the caller (main(), or a test) rather than here, so that
// caller can also hand the same ws.registry to startKafkaConsumers —
// newHandler itself stays pure HTTP wiring with no Kafka side effect,
// safe to call repeatedly from tests.
func newHandler(jwtSecret string, ws *wsServer) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		json.NewEncoder(w).Encode(map[string]string{"service": "gateway"})
	})
	mux.HandleFunc("/healthz", health.Handler("gateway"))

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

	// /profile, /profile/avatar/upload-url, /profile/avatar/confirm all
	// live on auth-svc's own mux as top-level paths (see
	// services/auth-svc/main.go), not nested under an internal "/profile"
	// prefix — same shape as transfers-svc's /deposits above, and for the
	// same reason forwarded unstripped rather than through the routes()
	// loop's newProxy. The exact "/profile" registration matters even more
	// here than for "/deposits": a redirect on GET is comparatively benign,
	// but PATCH /profile hitting only a "/profile/" subtree registration
	// would 301 to "/profile/", and fetch() demotes a redirected PATCH to
	// GET and drops the body — silently turning a display_name update into
	// a no-op read. (auth-svc is also still reachable the old way, under
	// "/auth/profile" via the routes() loop below — this is a second,
	// intentionally shorter path to the same handlers, not a replacement.)
	authAddr := envOr("AUTH_SVC_ADDR", "auth-svc:8081")
	noStripAuthProxy := newNoStripProxy(authAddr)
	mux.Handle("/profile", noStripAuthProxy)
	mux.Handle("/profile/", noStripAuthProxy)

	// Tracing wraps the JWT middleware rather than sitting inside it, so
	// the span covers authentication too: a request rejected with 401
	// still produces a trace showing where it died. The reverse order
	// would make every rejected request invisible, which is exactly the
	// kind of request someone goes looking for.
	return tracing.Handler(jwtMiddleware(mux, jwtSecret), "gateway")
}
