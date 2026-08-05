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

	log.Printf("gateway listening on :%s", port)
	if err := http.ListenAndServe(":"+port, newHandler(jwtSecret)); err != nil {
		log.Fatal(err)
	}
}

// newHandler builds the full gateway handler (routing + JWT middleware),
// separated from main() so gateway_test.go can exercise real routing
// behavior (redirects, prefix stripping, the public webhook allowlist)
// against an httptest.Server without a live JWT_SECRET/port bind.
func newHandler(jwtSecret string) http.Handler {
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
