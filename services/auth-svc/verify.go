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
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"neobank/pkg/outbox"
	eventsv1 "neobank/proto/gen/go/events/v1"
)

// authOutboxTable is auth-svc's outbox table name, shared with the outbox
// relay/cleanup workers wired up in main.go.
const authOutboxTable = "auth_outbox"

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
			writeJSONError(w, http.StatusBadRequest, "invalid_request_body")
			return
		}

		ctx := r.Context()
		outcome, _, attemptsRemaining, err := verifyEmailCode(ctx, pool, req.Email, req.Code)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error")
			return
		}

		switch outcome {
		case verifyOK:
			// The UserActivated event was already written to the outbox in
			// the same transaction that flipped status='active' (see
			// verifyEmailCode) — publishing to Kafka itself happens later,
			// asynchronously, via the outbox relay (main.go). Nothing left
			// to do here but report success.
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"status": "active"})
		case verifyUserNotFound:
			writeJSONError(w, http.StatusBadRequest, "user_not_found")
		case verifyNoActiveCode:
			writeJSONError(w, http.StatusBadRequest, "no_active_verification_code")
		case verifyCodeExpired:
			writeJSONError(w, http.StatusBadRequest, "verification_code_expired")
		case verifyTooManyAttempts:
			writeJSONError(w, http.StatusBadRequest, "too_many_verification_attempts")
		case verifyWrongCode:
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{
				"error":              "invalid_verification_code",
				"attempts_remaining": attemptsRemaining,
			})
		}
	}
}

func verifyEmailCode(ctx context.Context, pool *pgxpool.Pool, email, code string) (outcome verifyOutcome, userID string, attemptsRemaining int, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, "", 0, err
	}
	defer tx.Rollback(ctx)

	if err := tx.QueryRow(ctx, "SELECT id FROM users WHERE email = $1", email).Scan(&userID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return verifyUserNotFound, "", 0, nil
		}
		return 0, "", 0, err
	}

	outcome, attemptsRemaining, err = consumeVerificationCode(ctx, tx, userID, "email_verify", code)
	if err != nil {
		return 0, "", 0, err
	}
	if outcome != verifyOK {
		return outcome, "", attemptsRemaining, tx.Commit(ctx)
	}

	if _, err := tx.Exec(ctx, "UPDATE users SET status = 'active', updated_at = now() WHERE id = $1", userID); err != nil {
		return 0, "", 0, err
	}

	// The UserActivated event is written to the outbox in this same
	// transaction — either the activation and the event both commit, or
	// neither does. This is the fix for the dual-write gap that used to
	// live in kafka.go's publishUserActivated: publishing directly to
	// Kafka after this transaction committed meant a crash, or Kafka
	// being unreachable, between the two could leave the activation
	// permanently unannounced. The outbox relay (main.go) is what
	// actually gets this row to Kafka, asynchronously, sometime after
	// this function returns.
	eventID, err := outbox.GenerateEventID()
	if err != nil {
		return 0, "", 0, err
	}
	payload, err := proto.Marshal(&eventsv1.UserActivated{
		UserId:     userID,
		Email:      email,
		OccurredAt: timestamppb.New(time.Now()),
		EventId:    eventID,
	})
	if err != nil {
		return 0, "", 0, err
	}
	if err := outbox.InsertEvent(ctx, tx, authOutboxTable, eventID, "UserActivated", userID, payload); err != nil {
		return 0, "", 0, err
	}

	return verifyOK, userID, 0, tx.Commit(ctx)
}

// consumeVerificationCode looks up the newest unused verification_codes row
// for (userID, purpose), checks its expiry and remaining attempts, and
// compares the supplied code against the stored hash — decrementing
// attempts_remaining on a wrong guess or marking the row used on a match.
// The returned attemptsRemaining is only meaningful for verifyWrongCode (the
// post-decrement count the client should be told about); other outcomes
// return 0. Callers must already be inside a transaction and are
// responsible for committing and for any purpose-specific side effect on
// verifyOK.
func consumeVerificationCode(ctx context.Context, tx pgx.Tx, userID, purpose, code string) (verifyOutcome, int, error) {
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
		return verifyNoActiveCode, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}

	if time.Now().After(expiresAt) {
		return verifyCodeExpired, 0, nil
	}
	if attemptsRemaining <= 0 {
		return verifyTooManyAttempts, 0, nil
	}

	if hashCode(code) != storedHash {
		var remaining int
		if err := tx.QueryRow(ctx,
			"UPDATE verification_codes SET attempts_remaining = attempts_remaining - 1 WHERE id = $1 RETURNING attempts_remaining",
			codeID,
		).Scan(&remaining); err != nil {
			return 0, 0, err
		}
		return verifyWrongCode, remaining, nil
	}

	if _, err := tx.Exec(ctx, "UPDATE verification_codes SET used_at = now() WHERE id = $1", codeID); err != nil {
		return 0, 0, err
	}
	return verifyOK, 0, nil
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
			writeJSONError(w, http.StatusBadRequest, "invalid_request_body")
			return
		}
		if _, err := mail.ParseAddress(req.Email); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_email")
			return
		}

		ctx := r.Context()

		allowed, err := rdb.SetNX(ctx, "resend-verification:"+req.Email, "1", resendCooldown).Result()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error")
			return
		}
		if !allowed {
			writeJSONError(w, http.StatusTooManyRequests, "verification_code_cooldown")
			return
		}

		code, err := generateCode()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error")
			return
		}

		outcome, err := resendVerificationCode(ctx, pool, req.Email, hashCode(code))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error")
			return
		}
		switch outcome {
		case resendUserNotFound:
			writeJSONError(w, http.StatusBadRequest, "user_not_found")
			return
		case resendNotPending:
			writeJSONError(w, http.StatusBadRequest, "email_already_verified")
			return
		}

		if err := sendVerificationEmail(smtpAddr, smtpFrom, req.Email, code); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "verification_email_send_failed")
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
