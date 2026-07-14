package main

import (
	"encoding/json"
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

		claims := &accessTokenClaims{}
		_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (interface{}, error) {
			return []byte(secret), nil
		}, jwt.WithValidMethods([]string{"HS256"}))
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			writeJSONError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		if claims.UserID == "" {
			w.Header().Set("Content-Type", "application/json")
			writeJSONError(w, http.StatusUnauthorized, "invalid token claims")
			return
		}
		r.Header.Set("X-User-Id", claims.UserID)

		next.ServeHTTP(w, r)
	})
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
