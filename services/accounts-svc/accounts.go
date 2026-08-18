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
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"neobank/pkg/iban"
	"neobank/pkg/outbox"
	eventsv1 "neobank/proto/gen/go/events/v1"
)

// accountsOutboxTable is accounts-svc's outbox table name, shared with the
// outbox relay/cleanup workers wired up in main.go.
const accountsOutboxTable = "accounts_outbox"

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
	IBAN          string    `json:"iban"`
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

type accountCreateOutcome int

const (
	accountCreated accountCreateOutcome = iota
	accountAlreadyExists
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
// sign anything is wrong. It returns the account's id in both outcomes:
// handleUserActivated needs it to create the matching ledger account, and on
// a redelivery (accountAlreadyExists) that ledger call may still be pending,
// so the id is required there too, not just on a fresh create.
//
// A collision on user_id (accounts.user_id UNIQUE) is idempotency layer 1:
// the INSERT targets it explicitly via ON CONFLICT (user_id) DO NOTHING, so
// a redelivered UserActivated event (at-least-once Kafka semantics, or a
// crash after insert but before offset commit) is a safe, logged no-op —
// accountAlreadyExists, not an error. On that conflict DO NOTHING inserts no
// row, so RETURNING yields none; a follow-up SELECT fetches the existing id.
// Layer 2 (the processed_events table, see handleUserActivated in kafka.go)
// is a faster-path complement to this, not a replacement: this layer alone is
// what actually prevents a duplicate row from ever being created, in every
// case including ones layer 2's bookkeeping doesn't fully cover.
//
// The account-number-collision retry lives here, one attempt per call to
// tryCreateAccount below — each attempt is its own transaction, since
// Postgres aborts an entire transaction on any statement error (including
// this expected, retried collision) and re-generating inside a still-open,
// already-errored transaction isn't an option without SAVEPOINTs.
func createAccountForUser(ctx context.Context, pool *pgxpool.Pool, userID, bankCode string) (accountCreateOutcome, string, error) {
	for attempt := 0; attempt < maxAccountNumberAttempts; attempt++ {
		accountNumber, err := generateAccountNumber()
		if err != nil {
			return 0, "", fmt.Errorf("generate account number: %w", err)
		}

		sortCode, bbanAcctNum := ibanPartsFromAccountNumber(accountNumber)
		ibanValue, err := iban.Generate(bankCode, sortCode, bbanAcctNum)
		if err != nil {
			return 0, "", fmt.Errorf("generate iban: %w", err)
		}

		outcome, accountID, err := tryCreateAccount(ctx, pool, userID, accountNumber, ibanValue)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation &&
				pgErr.ConstraintName == accountsAccountNumberConstraint {
				continue // regenerate and retry
			}
			return 0, "", err
		}
		return outcome, accountID, nil
	}
	return 0, "", fmt.Errorf("failed to generate a unique account number after %d attempts", maxAccountNumberAttempts)
}

// tryCreateAccount makes one INSERT ... ON CONFLICT (user_id) DO NOTHING
// attempt inside its own transaction, writing an AccountCreated outbox
// event in that SAME transaction — but only when the row is actually
// freshly created. A redelivery that finds the account already there
// (accountAlreadyExists) must not re-publish the event a second time:
// AccountCreated already went out the first time this account was made.
func tryCreateAccount(ctx context.Context, pool *pgxpool.Pool, userID, accountNumber, ibanValue string) (accountCreateOutcome, string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback(ctx)

	var accountID string
	err = tx.QueryRow(ctx,
		"INSERT INTO accounts (user_id, account_number, iban) VALUES ($1, $2, $3) ON CONFLICT (user_id) DO NOTHING RETURNING id",
		userID, accountNumber, ibanValue,
	).Scan(&accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		// user_id conflict: the account already exists. Fetch its id — no
		// outbox write here, see the doc comment above.
		var existingID string
		if serr := tx.QueryRow(ctx, "SELECT id FROM accounts WHERE user_id = $1", userID).Scan(&existingID); serr != nil {
			return 0, "", fmt.Errorf("look up existing account for user %s: %w", userID, serr)
		}
		return accountAlreadyExists, existingID, tx.Commit(ctx)
	}
	if err != nil {
		return 0, "", err
	}

	eventID, err := outbox.GenerateEventID()
	if err != nil {
		return 0, "", fmt.Errorf("generate event id: %w", err)
	}
	payload, err := proto.Marshal(&eventsv1.AccountCreated{
		EventId:       eventID,
		UserId:        userID,
		AccountId:     accountID,
		AccountNumber: accountNumber,
		OccurredAt:    timestamppb.New(time.Now()),
	})
	if err != nil {
		return 0, "", fmt.Errorf("marshal AccountCreated event: %w", err)
	}
	if err := outbox.InsertEvent(ctx, tx, accountsOutboxTable, eventID, "AccountCreated", userID, payload); err != nil {
		return 0, "", fmt.Errorf("insert outbox event: %w", err)
	}

	return accountCreated, accountID, tx.Commit(ctx)
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

// ibanPartsFromAccountNumber deterministically and injectively derives the
// BBAN sort-code and account-number fields from accountNumber's
// accountNumberDigits digits (after the "NB" prefix): the first 2 become
// the sort code's low digits, the remaining 8 become the BBAN
// account-number field verbatim. Injective because it uses every digit
// exactly once with no hashing — so two distinct account numbers can never
// derive the same IBAN, meaning accounts.iban's uniqueness follows
// automatically from accounts.account_number's uniqueness (itself already
// enforced, see createAccountForUser), with no additional collision-retry
// loop needed for the IBAN specifically.
func ibanPartsFromAccountNumber(accountNumber string) (sortCode, bbanAccountNumber string) {
	digits := accountNumber[len(accountNumberPrefix):]
	return "0000" + digits[0:2], digits[2:10]
}

func getAccountByUserID(ctx context.Context, pool *pgxpool.Pool, userID string) (Account, bool, error) {
	var acc Account
	err := pool.QueryRow(ctx,
		"SELECT id, user_id, account_number, iban, status, created_at, updated_at FROM accounts WHERE user_id = $1",
		userID,
	).Scan(&acc.ID, &acc.UserID, &acc.AccountNumber, &acc.IBAN, &acc.Status, &acc.CreatedAt, &acc.UpdatedAt)
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
		"SELECT id, user_id, account_number, iban, status, created_at, updated_at FROM accounts WHERE id = $1",
		id,
	).Scan(&acc.ID, &acc.UserID, &acc.AccountNumber, &acc.IBAN, &acc.Status, &acc.CreatedAt, &acc.UpdatedAt)
	if isNotFoundErr(err) {
		return Account{}, false, nil
	}
	if err != nil {
		return Account{}, false, err
	}
	return acc, true, nil
}

// getAccountsByIDs returns whatever accounts among ids exist, in no
// particular order, silently omitting any id with no matching row — unlike
// the single-row lookups above ((Account, bool, error), exactly one id), a
// batch caller needs partial results, not an all-or-nothing failure over
// one bad id in a larger list.
func getAccountsByIDs(ctx context.Context, pool *pgxpool.Pool, ids []string) ([]Account, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx,
		"SELECT id, user_id, account_number, iban, status, created_at, updated_at FROM accounts WHERE id = ANY($1::uuid[])",
		ids,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []Account
	for rows.Next() {
		var acc Account
		if err := rows.Scan(&acc.ID, &acc.UserID, &acc.AccountNumber, &acc.IBAN, &acc.Status, &acc.CreatedAt, &acc.UpdatedAt); err != nil {
			return nil, err
		}
		accounts = append(accounts, acc)
	}
	return accounts, rows.Err()
}

func getAccountByAccountNumber(ctx context.Context, pool *pgxpool.Pool, accountNumber string) (Account, bool, error) {
	var acc Account
	err := pool.QueryRow(ctx,
		"SELECT id, user_id, account_number, iban, status, created_at, updated_at FROM accounts WHERE account_number = $1",
		accountNumber,
	).Scan(&acc.ID, &acc.UserID, &acc.AccountNumber, &acc.IBAN, &acc.Status, &acc.CreatedAt, &acc.UpdatedAt)
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
		 RETURNING id, user_id, account_number, iban, status, created_at, updated_at`,
		newStatus, id,
	).Scan(&acc.ID, &acc.UserID, &acc.AccountNumber, &acc.IBAN, &acc.Status, &acc.CreatedAt, &acc.UpdatedAt)
	if err != nil {
		return Account{}, 0, err
	}
	return acc, statusUpdateOK, tx.Commit(ctx)
}

// isEventProcessed reports whether eventID is already recorded in
// processed_events — idempotency layer 2's fast-path check, see
// handleUserActivated in kafka.go for how this combines with layer 1.
func isEventProcessed(ctx context.Context, pool *pgxpool.Pool, eventID string) (bool, error) {
	var exists bool
	err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM processed_events WHERE event_id = $1)",
		eventID,
	).Scan(&exists)
	return exists, err
}

// markEventProcessed records eventID as processed. ON CONFLICT DO NOTHING
// isn't load-bearing for the current single-instance consumer (there's
// never a concurrent call for the same event within one process), but is
// cheap insurance against a future multi-replica deployment turning a
// duplicate bookkeeping write into a crash instead of a harmless no-op.
func markEventProcessed(ctx context.Context, pool *pgxpool.Pool, eventID string) error {
	_, err := pool.Exec(ctx,
		"INSERT INTO processed_events (event_id) VALUES ($1) ON CONFLICT (event_id) DO NOTHING",
		eventID,
	)
	return err
}
