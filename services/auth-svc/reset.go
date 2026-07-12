package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"net/smtp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

const forgotPasswordCooldown = 60 * time.Second

func forgotPasswordHandler(pool *pgxpool.Pool, rdb *redis.Client, smtpAddr, smtpFrom string) http.HandlerFunc {
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

		allowed, err := rdb.SetNX(ctx, "forgot-password:"+req.Email, "1", forgotPasswordCooldown).Result()
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

		issued, err := issuePasswordResetCode(ctx, pool, req.Email, hashCode(code))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}

		// Only ever send mail when the account actually exists, but always
		// return the same response either way — and never let a send
		// failure surface as a different status, or "exists but SMTP
		// flaked" becomes distinguishable from "doesn't exist".
		if issued {
			if err := sendPasswordResetEmail(smtpAddr, smtpFrom, req.Email, code); err != nil {
				log.Printf("auth-svc: failed to send password reset email to %s: %v", req.Email, err)
			}
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"message": "if the email is registered, a reset code has been sent"})
	}
}

// issuePasswordResetCode issues a password_reset verification code for the
// user with the given email, if one exists. issued is false, with no error,
// when the email doesn't match any user.
func issuePasswordResetCode(ctx context.Context, pool *pgxpool.Pool, email, codeHash string) (issued bool, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var userID string
	err = tx.QueryRow(ctx, "SELECT id FROM users WHERE email = $1 FOR UPDATE", email).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if err := invalidateAndIssueCode(ctx, tx, userID, "password_reset", codeHash); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func sendPasswordResetEmail(addr, from, to, code string) error {
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: Reset your Neo-Bank password\r\n\r\n"+
		"Your password reset code is: %s\r\nIt expires in 10 minutes.\r\n", from, to, code)
	return smtp.SendMail(addr, nil, from, []string{to}, []byte(msg))
}

func resetPasswordHandler(pool *pgxpool.Pool, rdb *redis.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var req struct {
			Email       string `json:"email"`
			Code        string `json:"code"`
			NewPassword string `json:"new_password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if len(req.NewPassword) < 8 {
			writeJSONError(w, http.StatusBadRequest, "password must be at least 8 characters")
			return
		}

		ctx := r.Context()
		outcome, userID, err := resetPassword(ctx, pool, req.Email, req.Code, req.NewPassword)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}

		switch outcome {
		case verifyUserNotFound, verifyNoActiveCode:
			// Merged on purpose: unlike /verify-email (only ever reached
			// right after /register, which already reveals registration
			// status via its own 409), this endpoint is reachable cold with
			// a guessed code for any email. Distinguishing "no such user"
			// from "no code was ever requested" would reopen the exact
			// enumeration hole /forgot-password's uniform response closes.
			writeJSONError(w, http.StatusBadRequest, "invalid or expired reset code")
			return
		case verifyCodeExpired:
			writeJSONError(w, http.StatusBadRequest, "verification code has expired")
			return
		case verifyTooManyAttempts:
			writeJSONError(w, http.StatusBadRequest, "too many failed attempts, request a new code")
			return
		case verifyWrongCode:
			writeJSONError(w, http.StatusBadRequest, "invalid verification code")
			return
		}

		// The password change already committed — that's the guarantee
		// that matters. A Redis hiccup here shouldn't turn into a 500 that
		// falsely tells the client their reset failed; log it instead so
		// it's visible that old sessions may still be live.
		if err := revokeAllUserRefreshTokens(ctx, rdb, userID); err != nil {
			log.Printf("auth-svc: failed to revoke refresh tokens for user %s after password reset: %v", userID, err)
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "password_reset"})
	}
}

// resetPassword validates a password_reset code exactly like
// consumeVerificationCode does for email verification, and on success
// updates the user's password_hash. The returned userID is only valid when
// outcome is verifyOK.
func resetPassword(ctx context.Context, pool *pgxpool.Pool, email, code, newPassword string) (outcome verifyOutcome, userID string, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback(ctx)

	if err := tx.QueryRow(ctx, "SELECT id FROM users WHERE email = $1", email).Scan(&userID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return verifyUserNotFound, "", nil
		}
		return 0, "", err
	}

	outcome, err = consumeVerificationCode(ctx, tx, userID, "password_reset", code)
	if err != nil {
		return 0, "", err
	}
	if outcome != verifyOK {
		return outcome, "", tx.Commit(ctx)
	}

	newHash, err := hashPassword(newPassword)
	if err != nil {
		return 0, "", err
	}
	if _, err := tx.Exec(ctx, "UPDATE users SET password_hash = $1, updated_at = now() WHERE id = $2", newHash, userID); err != nil {
		return 0, "", err
	}

	return verifyOK, userID, tx.Commit(ctx)
}

// revokeAllUserRefreshTokens deletes every outstanding refresh token issued
// to userID, plus the tracking set itself, forcing re-login everywhere.
func revokeAllUserRefreshTokens(ctx context.Context, rdb *redis.Client, userID string) error {
	setKey := userRefreshKeyPrefix + userID
	tokens, err := rdb.SMembers(ctx, setKey).Result()
	if err != nil {
		return err
	}

	keys := make([]string, 0, len(tokens)+1)
	for _, token := range tokens {
		keys = append(keys, refreshKeyPrefix+token)
	}
	keys = append(keys, setKey)

	return rdb.Del(ctx, keys...).Err()
}
