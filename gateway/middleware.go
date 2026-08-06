package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

var publicPaths = map[string]struct{}{
	"/":                         {},
	"/healthz":                  {},
	"/auth/register":            {},
	"/auth/verify-email":        {},
	"/auth/resend-verification": {},
	"/auth/login":               {},
	"/auth/refresh":             {},
	"/auth/forgot-password":     {},
	"/auth/reset-password":      {},
	// Stripe has no JWT to send — its authentication IS the webhook
	// signature (verified in transfers-svc/webhook.go), not a bearer
	// token. This must stay a public route, never gated behind JWT.
	"/webhooks/stripe": {},
	// The browser WebSocket API cannot set an Authorization header on the
	// handshake request, so /ws can't be gated here the way every other
	// route is. Instead ws.go authenticates the connection itself via the
	// first message the client sends after upgrade (see wsServer.handleWS)
	// — this route bypasses only the header check, not authentication.
	"/ws": {},
}

type accessTokenClaims struct {
	UserID string `json:"user_id"`
	jwt.RegisteredClaims
}

func jwtMiddleware(next http.Handler, secret string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Internal services (accounts-svc today) trust X-User-Id as the
		// caller's identity without re-verifying the JWT themselves, so an
		// external client must never be able to set it directly — strip
		// whatever the client sent before deciding anything else, on every
		// request, not just authenticated ones.
		r.Header.Del("X-User-Id")

		if _, ok := publicPaths[r.URL.Path]; ok {
			next.ServeHTTP(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(authHeader, "Bearer ")
		if !ok || token == "" {
			w.Header().Set("Content-Type", "application/json")
			writeJSONError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}

		claims, err := parseAccessToken(token, secret)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			writeJSONError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		r.Header.Set("X-User-Id", claims.UserID)

		next.ServeHTTP(w, r)
	})
}

// parseAccessToken validates a bearer token against secret and returns its
// claims. Shared by jwtMiddleware (Authorization header) and ws.go's
// handshake auth message — both need identical validation, just fed from
// different places since a browser WebSocket handshake can't carry a
// header.
func parseAccessToken(tokenString, secret string) (*accessTokenClaims, error) {
	claims := &accessTokenClaims{}
	_, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return nil, err
	}
	if claims.UserID == "" {
		return nil, errors.New("token missing user_id claim")
	}
	return claims, nil
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
