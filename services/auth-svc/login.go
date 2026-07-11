package main

import (
	"context"
	crand "crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/argon2"
)

const (
	accessTokenTTL  = 15 * time.Minute
	refreshTokenTTL = 7 * 24 * time.Hour

	refreshKeyPrefix = "refresh:"
)

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type logoutRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type tokenPairResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

type authUser struct {
	ID           string
	Email        string
	PasswordHash string
	Status       string
}

type accessTokenClaims struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	jwt.RegisteredClaims
}

type loginOutcome int

const (
	loginOK loginOutcome = iota
	loginInvalidCredentials
	loginNotVerified
)

type refreshOutcome int

const (
	refreshOK refreshOutcome = iota
	refreshInvalidToken
	refreshAccountNotActive
)

// dummyPasswordHash lets authenticateUser run a real argon2 comparison even
// when the user doesn't exist, so response timing can't reveal whether an
// email is registered.
var dummyPasswordHash string

func init() {
	hash, err := hashPassword("timing-safety-placeholder")
	if err != nil {
		panic("auth-svc: failed to precompute dummy password hash: " + err.Error())
	}
	dummyPasswordHash = hash
}

func loginHandler(pool *pgxpool.Pool, rdb *redis.Client, jwtSecret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var req loginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		ctx := r.Context()
		user, outcome, err := authenticateUser(ctx, pool, req.Email, req.Password)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}

		switch outcome {
		case loginInvalidCredentials:
			writeJSONError(w, http.StatusUnauthorized, "invalid credentials")
			return
		case loginNotVerified:
			writeJSONError(w, http.StatusForbidden, "email not verified")
			return
		}

		pair, err := issueTokenPair(ctx, rdb, jwtSecret, user)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(pair)
	}
}

func refreshHandler(pool *pgxpool.Pool, rdb *redis.Client, jwtSecret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var req refreshRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		ctx := r.Context()
		user, outcome, err := consumeRefreshToken(ctx, rdb, pool, req.RefreshToken)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}

		switch outcome {
		case refreshInvalidToken:
			writeJSONError(w, http.StatusUnauthorized, "invalid or expired refresh token")
			return
		case refreshAccountNotActive:
			writeJSONError(w, http.StatusUnauthorized, "account is no longer active")
			return
		}

		pair, err := issueTokenPair(ctx, rdb, jwtSecret, user)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(pair)
	}
}

func logoutHandler(rdb *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var req logoutRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		if err := rdb.Del(r.Context(), refreshKeyPrefix+req.RefreshToken).Err(); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "logged_out"})
	}
}

// authenticateUser always performs a password comparison, using
// dummyPasswordHash when no user matches email, so a wrong-email and a
// wrong-password attempt take the same code path and (approximately) the
// same time. status is only inspected once the password has been verified,
// so a wrong password against a pending_verification account still yields
// loginInvalidCredentials rather than leaking loginNotVerified.
func authenticateUser(ctx context.Context, pool *pgxpool.Pool, email, password string) (authUser, loginOutcome, error) {
	user, found, err := findUserByEmail(ctx, pool, email)
	if err != nil {
		return authUser{}, 0, err
	}

	hash := dummyPasswordHash
	if found {
		hash = user.PasswordHash
	}

	ok, err := verifyPassword(password, hash)
	if err != nil {
		return authUser{}, 0, err
	}
	if !found || !ok {
		return authUser{}, loginInvalidCredentials, nil
	}
	if user.Status != "active" {
		return authUser{}, loginNotVerified, nil
	}
	return user, loginOK, nil
}

// consumeRefreshToken atomically pops "refresh:<token>" from Redis so a
// given token can never be redeemed twice, then re-resolves the owning
// user to pick up the current status (e.g. if the account was suspended
// after the token was issued).
func consumeRefreshToken(ctx context.Context, rdb *redis.Client, pool *pgxpool.Pool, token string) (authUser, refreshOutcome, error) {
	userID, err := rdb.GetDel(ctx, refreshKeyPrefix+token).Result()
	if errors.Is(err, redis.Nil) {
		return authUser{}, refreshInvalidToken, nil
	}
	if err != nil {
		return authUser{}, 0, err
	}

	user, found, err := findUserByID(ctx, pool, userID)
	if err != nil {
		return authUser{}, 0, err
	}
	if !found || user.Status != "active" {
		return authUser{}, refreshAccountNotActive, nil
	}
	return user, refreshOK, nil
}

func findUserByEmail(ctx context.Context, pool *pgxpool.Pool, email string) (authUser, bool, error) {
	var user authUser
	err := pool.QueryRow(ctx,
		"SELECT id, email, password_hash, status FROM users WHERE email = $1",
		email,
	).Scan(&user.ID, &user.Email, &user.PasswordHash, &user.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return authUser{}, false, nil
	}
	if err != nil {
		return authUser{}, false, err
	}
	return user, true, nil
}

func findUserByID(ctx context.Context, pool *pgxpool.Pool, id string) (authUser, bool, error) {
	var user authUser
	err := pool.QueryRow(ctx,
		"SELECT id, email, password_hash, status FROM users WHERE id = $1",
		id,
	).Scan(&user.ID, &user.Email, &user.PasswordHash, &user.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return authUser{}, false, nil
	}
	if err != nil {
		return authUser{}, false, err
	}
	return user, true, nil
}

// issueTokenPair mints a fresh access+refresh pair for user and stores the
// refresh token in Redis; it's the single place both login and refresh use
// so the two never drift apart.
func issueTokenPair(ctx context.Context, rdb *redis.Client, jwtSecret string, user authUser) (tokenPairResponse, error) {
	accessToken, err := newAccessToken(jwtSecret, user.ID, user.Email)
	if err != nil {
		return tokenPairResponse{}, err
	}

	refreshToken, err := generateRefreshToken()
	if err != nil {
		return tokenPairResponse{}, err
	}

	if err := rdb.Set(ctx, refreshKeyPrefix+refreshToken, user.ID, refreshTokenTTL).Err(); err != nil {
		return tokenPairResponse{}, err
	}

	return tokenPairResponse{AccessToken: accessToken, RefreshToken: refreshToken}, nil
}

func newAccessToken(jwtSecret, userID, email string) (string, error) {
	now := time.Now()
	claims := accessTokenClaims{
		UserID: userID,
		Email:  email,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(accessTokenTTL)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(jwtSecret))
}

func generateRefreshToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := crand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// verifyPassword parses the PHC-formatted hash produced by hashPassword
// ($argon2id$v=%d$m=%d,t=%d,p=%d$salt$hash), re-derives a hash for password
// using the parameters embedded in encodedHash, and compares in constant
// time.
func verifyPassword(password, encodedHash string) (bool, error) {
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("unsupported password hash format")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, err
	}

	var memory, timeCost uint32
	var threads uint8
	params := strings.Split(parts[3], ",")
	if len(params) != 3 {
		return false, errors.New("unsupported password hash format")
	}
	for _, p := range params {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			return false, errors.New("unsupported password hash format")
		}
		val, err := strconv.ParseUint(kv[1], 10, 32)
		if err != nil {
			return false, err
		}
		switch kv[0] {
		case "m":
			memory = uint32(val)
		case "t":
			timeCost = uint32(val)
		case "p":
			threads = uint8(val)
		default:
			return false, errors.New("unsupported password hash format")
		}
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	storedHash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, err
	}

	computedHash := argon2.IDKey([]byte(password), salt, timeCost, memory, threads, uint32(len(storedHash)))
	return subtle.ConstantTimeCompare(storedHash, computedHash) == 1, nil
}
