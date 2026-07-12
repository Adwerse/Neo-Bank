package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const resendCooldown = 60 * time.Second

type verifyOutcome int

const (
	verifyOK verifyOutcome = iota
	verifyUserNotFound
	verifyNoActiveCode
	verifyCodeExpired
	verifyTooManyAttempts
	verifyWrongCode
)

func verifyEmailHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var req struct {
			Email string `json:"email"`
			Code  string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		outcome, err := verifyEmailCode(r.Context(), pool, req.Email, req.Code)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}

		switch outcome {
		case verifyOK:
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"status": "active"})
		case verifyUserNotFound:
			writeJSONError(w, http.StatusBadRequest, "user not found")
		case verifyNoActiveCode:
			writeJSONError(w, http.StatusBadRequest, "no active verification code, request a new one")
		case verifyCodeExpired:
			writeJSONError(w, http.StatusBadRequest, "verification code has expired")
		case verifyTooManyAttempts:
			writeJSONError(w, http.StatusBadRequest, "too many failed attempts, request a new code")
		case verifyWrongCode:
			writeJSONError(w, http.StatusBadRequest, "invalid verification code")
		}
	}
}

func verifyEmailCode(ctx context.Context, pool *pgxpool.Pool, email, code string) (verifyOutcome, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var userID string
	if err := tx.QueryRow(ctx, "SELECT id FROM users WHERE email = $1", email).Scan(&userID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return verifyUserNotFound, nil
		}
		return 0, err
	}

	outcome, err := consumeVerificationCode(ctx, tx, userID, "email_verify", code)
	if err != nil {
		return 0, err
	}
	if outcome != verifyOK {
		return outcome, tx.Commit(ctx)
	}

	if _, err := tx.Exec(ctx, "UPDATE users SET status = 'active', updated_at = now() WHERE id = $1", userID); err != nil {
		return 0, err
	}
	return verifyOK, tx.Commit(ctx)
}

// consumeVerificationCode looks up the newest unused verification_codes row
// for (userID, purpose), checks its expiry and remaining attempts, and
// compares the supplied code against the stored hash — decrementing
// attempts_remaining on a wrong guess or marking the row used on a match.
// Callers must already be inside a transaction and are responsible for
// committing and for any purpose-specific side effect on verifyOK.
func consumeVerificationCode(ctx context.Context, tx pgx.Tx, userID, purpose, code string) (verifyOutcome, error) {
	var codeID, storedHash string
	var expiresAt time.Time
	var attemptsRemaining int
	err := tx.QueryRow(ctx,
		`SELECT id, code_hash, expires_at, attempts_remaining
		 FROM verification_codes
		 WHERE user_id = $1 AND purpose = $2 AND used_at IS NULL
		 ORDER BY created_at DESC LIMIT 1 FOR UPDATE`,
		userID, purpose,
	).Scan(&codeID, &storedHash, &expiresAt, &attemptsRemaining)
	if errors.Is(err, pgx.ErrNoRows) {
		return verifyNoActiveCode, nil
	}
	if err != nil {
		return 0, err
	}

	if time.Now().After(expiresAt) {
		return verifyCodeExpired, nil
	}
	if attemptsRemaining <= 0 {
		return verifyTooManyAttempts, nil
	}

	if hashCode(code) != storedHash {
		if _, err := tx.Exec(ctx,
			"UPDATE verification_codes SET attempts_remaining = attempts_remaining - 1 WHERE id = $1",
			codeID,
		); err != nil {
			return 0, err
		}
		return verifyWrongCode, nil
	}

	if _, err := tx.Exec(ctx, "UPDATE verification_codes SET used_at = now() WHERE id = $1", codeID); err != nil {
		return 0, err
	}
	return verifyOK, nil
}

type resendOutcome int

const (
	resendOK resendOutcome = iota
	resendUserNotFound
	resendNotPending
)

func resendVerificationHandler(pool *pgxpool.Pool, rdb *redis.Client, smtpAddr, smtpFrom string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var req struct {
			Email string `json:"email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if _, err := mail.ParseAddress(req.Email); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid email")
			return
		}

		ctx := r.Context()

		allowed, err := rdb.SetNX(ctx, "resend-verification:"+req.Email, "1", resendCooldown).Result()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		if !allowed {
			writeJSONError(w, http.StatusTooManyRequests, "please wait before requesting another code")
			return
		}

		code, err := generateCode()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}

		outcome, err := resendVerificationCode(ctx, pool, req.Email, hashCode(code))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		switch outcome {
		case resendUserNotFound:
			writeJSONError(w, http.StatusBadRequest, "user not found")
			return
		case resendNotPending:
			writeJSONError(w, http.StatusBadRequest, "email is already verified")
			return
		}

		if err := sendVerificationEmail(smtpAddr, smtpFrom, req.Email, code); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to send verification email")
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "pending_verification"})
	}
}

func resendVerificationCode(ctx context.Context, pool *pgxpool.Pool, email, codeHash string) (resendOutcome, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var userID, status string
	err = tx.QueryRow(ctx, "SELECT id, status FROM users WHERE email = $1 FOR UPDATE", email).Scan(&userID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return resendUserNotFound, nil
	}
	if err != nil {
		return 0, err
	}
	if status != "pending_verification" {
		return resendNotPending, nil
	}

	if err := invalidateAndIssueCode(ctx, tx, userID, "email_verify", codeHash); err != nil {
		return 0, err
	}
	return resendOK, tx.Commit(ctx)
}
