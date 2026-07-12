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

func jwtMiddleware(next http.Handler, secret string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		_, err := jwt.ParseWithClaims(token, &jwt.RegisteredClaims{}, func(t *jwt.Token) (interface{}, error) {
			return []byte(secret), nil
		}, jwt.WithValidMethods([]string{"HS256"}))
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			writeJSONError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}

		next.ServeHTTP(w, r)
	})
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
