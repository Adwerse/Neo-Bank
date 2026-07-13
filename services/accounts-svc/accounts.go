package main

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	accountNumberPrefix      = "NB"
	accountNumberDigits      = 10
	maxAccountNumberAttempts = 10

	uniqueViolation           = "23505"
	invalidTextRepresentation = "22P02" // malformed input for a typed column, e.g. a non-UUID string bound to a UUID param

	// Postgres's default auto-generated names for single-column UNIQUE
	// constraints ("<table>_<column>_key"), per the accounts migration —
	// not explicitly named there, so this naming is implicit/derived.
	accountsAccountNumberConstraint = "accounts_account_number_key"
)

// Account is the JSON representation of an accounts row.
type Account struct {
	ID            string    `json:"id"`
	UserID        string    `json:"user_id"`
	AccountNumber string    `json:"account_number"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type accountStatusOutcome int

const (
	statusUpdateOK accountStatusOutcome = iota
	statusUpdateNotFound
	statusUpdateInvalidTransition
)

var validAccountStatuses = map[string]struct{}{
	"active": {}, "frozen": {}, "closed": {},
}

// isNotFoundErr treats a malformed id (e.g. a non-UUID path segment bound to
// accounts.id, which Postgres rejects with SQLSTATE 22P02 before it ever
// gets to "no rows") the same as a genuinely missing row — both are a 404
// to an HTTP caller. This case doesn't arise elsewhere in the repo today
// because auth-svc's id lookups only ever receive ids sourced from JWT
// claims, never a raw, untrusted URL path segment.
func isNotFoundErr(err error) bool {
	if errors.Is(err, pgx.ErrNoRows) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == invalidTextRepresentation
}

// createAccountForUser inserts a new accounts row for userID with a freshly
// generated account number, retrying with a newly generated number (up to
// maxAccountNumberAttempts times) if that number collides with an existing
// one — an expected, non-error outcome given the random number space, not a
// sign anything is wrong.
//
// A collision on user_id (accounts.user_id UNIQUE) is not retried or
// swallowed: it's returned to the caller like any other error. That user
// already has an account, almost certainly because this UserActivated
// event was redelivered (at-least-once Kafka semantics, or a crash after
// insert but before offset commit). No idempotency/dedup handling is added
// here — the caller logs the error and leaves the offset uncommitted, for
// a future idempotency layer to special-case.
func createAccountForUser(ctx context.Context, pool *pgxpool.Pool, userID string) error {
	for attempt := 0; attempt < maxAccountNumberAttempts; attempt++ {
		accountNumber, err := generateAccountNumber()
		if err != nil {
			return fmt.Errorf("generate account number: %w", err)
		}

		_, err = pool.Exec(ctx,
			"INSERT INTO accounts (user_id, account_number) VALUES ($1, $2)",
			userID, accountNumber,
		)
		if err == nil {
			return nil
		}

		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation &&
			pgErr.ConstraintName == accountsAccountNumberConstraint {
			continue // regenerate and retry
		}
		return err // user_id collision, or any other error — not retried
	}
	return fmt.Errorf("failed to generate a unique account number after %d attempts", maxAccountNumberAttempts)
}

// generateAccountNumber returns a synthetic account number of the form
// "NB" + accountNumberDigits zero-padded random digits (e.g.
// "NB0417235968"), mirroring generateCode's crypto/rand + big.Int style in
// auth-svc/register.go.
func generateAccountNumber() (string, error) {
	max := new(big.Int).Exp(big.NewInt(10), big.NewInt(accountNumberDigits), nil)
	n, err := crand.Int(crand.Reader, max)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%0*d", accountNumberPrefix, accountNumberDigits, n.Int64()), nil
}

func getAccountByUserID(ctx context.Context, pool *pgxpool.Pool, userID string) (Account, bool, error) {
	var acc Account
	err := pool.QueryRow(ctx,
		"SELECT id, user_id, account_number, status, created_at, updated_at FROM accounts WHERE user_id = $1",
		userID,
	).Scan(&acc.ID, &acc.UserID, &acc.AccountNumber, &acc.Status, &acc.CreatedAt, &acc.UpdatedAt)
	if isNotFoundErr(err) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, err
	}
	return acc, true, nil
}

func getAccountByID(ctx context.Context, pool *pgxpool.Pool, id string) (Account, bool, error) {
	var acc Account
	err := pool.QueryRow(ctx,
		"SELECT id, user_id, account_number, status, created_at, updated_at FROM accounts WHERE id = $1",
		id,
	).Scan(&acc.ID, &acc.UserID, &acc.AccountNumber, &acc.Status, &acc.CreatedAt, &acc.UpdatedAt)
	if isNotFoundErr(err) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, err
	}
	return acc, true, nil
}

// updateAccountStatus locks the account row, rejects any transition away
// from "closed" (terminal, including closed -> closed), and otherwise
// applies newStatus unconditionally — every other from-state may move to
// any of the three values. newStatus is trusted to already be one of the
// three valid values; callers validate that before calling in.
func updateAccountStatus(ctx context.Context, pool *pgxpool.Pool, id, newStatus string) (Account, accountStatusOutcome, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Account{}, 0, err
	}
	defer tx.Rollback(ctx)

	var currentStatus string
	err = tx.QueryRow(ctx, "SELECT status FROM accounts WHERE id = $1 FOR UPDATE", id).Scan(&currentStatus)
	if isNotFoundErr(err) {
		return Account{}, statusUpdateNotFound, nil
	}
	if err != nil {
		return Account{}, 0, err
	}
	if currentStatus == "closed" {
		return Account{}, statusUpdateInvalidTransition, nil
	}

	var acc Account
	err = tx.QueryRow(ctx,
		`UPDATE accounts SET status = $1, updated_at = now() WHERE id = $2
		 RETURNING id, user_id, account_number, status, created_at, updated_at`,
		newStatus, id,
	).Scan(&acc.ID, &acc.UserID, &acc.AccountNumber, &acc.Status, &acc.CreatedAt, &acc.UpdatedAt)
	if err != nil {
		return Account{}, 0, err
	}
	return acc, statusUpdateOK, tx.Commit(ctx)
}
