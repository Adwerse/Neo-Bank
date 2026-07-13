package main

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"math/big"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	accountNumberPrefix      = "NB"
	accountNumberDigits      = 10
	maxAccountNumberAttempts = 10

	uniqueViolation = "23505"

	// Postgres's default auto-generated names for single-column UNIQUE
	// constraints ("<table>_<column>_key"), per the accounts migration —
	// not explicitly named there, so this naming is implicit/derived.
	accountsAccountNumberConstraint = "accounts_account_number_key"
)

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
