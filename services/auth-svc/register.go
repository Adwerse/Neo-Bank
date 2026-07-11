package main

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/mail"
	"net/smtp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/argon2"
)

const (
	argonTime    = 2
	argonMemory  = 19 * 1024 // KiB
	argonThreads = 1
	argonKeyLen  = 32
	saltLen      = 16

	uniqueViolation = "23505"
)

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func registerHandler(pool *pgxpool.Pool, smtpAddr, smtpFrom string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var req registerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		if _, err := mail.ParseAddress(req.Email); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid email")
			return
		}
		if len(req.Password) < 8 {
			writeJSONError(w, http.StatusBadRequest, "password must be at least 8 characters")
			return
		}

		passwordHash, err := hashPassword(req.Password)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}

		code, err := generateCode()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}

		ctx := r.Context()
		conflict, err := registerUserAndIssueCode(ctx, pool, req.Email, passwordHash, hashCode(code))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		if conflict {
			writeJSONError(w, http.StatusConflict, "email already registered")
			return
		}

		if err := sendVerificationEmail(smtpAddr, smtpFrom, req.Email, code); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to send verification email")
			return
		}

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"status": "pending_verification"})
	}
}

// registerUserAndIssueCode creates a new user or, for one still pending
// verification, updates its password and reissues a code. It reports
// conflict=true when the email belongs to an active or suspended user.
func registerUserAndIssueCode(ctx context.Context, pool *pgxpool.Pool, email, passwordHash, codeHash string) (conflict bool, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var userID, status string
	err = tx.QueryRow(ctx, "SELECT id, status FROM users WHERE email = $1 FOR UPDATE", email).Scan(&userID, &status)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		err = tx.QueryRow(ctx,
			"INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING id",
			email, passwordHash,
		).Scan(&userID)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
				return true, nil
			}
			return false, err
		}

	case err != nil:
		return false, err

	case status != "pending_verification":
		return true, nil

	default:
		if _, err := tx.Exec(ctx,
			"UPDATE users SET password_hash = $1, updated_at = now() WHERE id = $2",
			passwordHash, userID,
		); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx,
			"UPDATE verification_codes SET used_at = now() WHERE user_id = $1 AND purpose = 'email_verify' AND used_at IS NULL",
			userID,
		); err != nil {
			return false, err
		}
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO verification_codes (user_id, purpose, code_hash, expires_at, attempts_remaining)
		 VALUES ($1, 'email_verify', $2, now() + interval '10 minutes', 5)`,
		userID, codeHash,
	); err != nil {
		return false, err
	}

	return false, tx.Commit(ctx)
}

func hashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := crand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	), nil
}

func generateCode() (string, error) {
	n, err := crand.Int(crand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func hashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

func sendVerificationEmail(addr, from, to, code string) error {
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: Verify your Neo-Bank email\r\n\r\n"+
		"Your verification code is: %s\r\nIt expires in 10 minutes.\r\n", from, to, code)
	return smtp.SendMail(addr, nil, from, []string{to}, []byte(msg))
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
