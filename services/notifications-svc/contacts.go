package main

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Status values for notifications_processed_events, matching the CHECK
// constraint in migration 000002 — see that file for what each means now
// that this service actually sends mail.
const (
	eventStatusProcessing = "processing"
	eventStatusSent       = "sent"
	eventStatusSkipped    = "skipped"
)

// contact is a user_contacts row as the transfer handlers need it: an
// address to mail, and the account number that may appear in the other
// party's email.
type contact struct {
	UserID        string
	Email         string
	AccountNumber string // "" when the column is NULL — see migration 000003
}

// upsertUserContactEmail records (or updates) userID's email in the
// user_contacts projection, from a UserActivated event. ON CONFLICT only
// touches email/updated_at — it must never clobber account_id, which is
// filled in separately (and possibly later) by updateUserContactAccountLink
// from an AccountCreated event.
func upsertUserContactEmail(ctx context.Context, pool *pgxpool.Pool, userID, email string) error {
	_, err := pool.Exec(ctx,
		`INSERT INTO user_contacts (user_id, email) VALUES ($1, $2)
		 ON CONFLICT (user_id) DO UPDATE SET email = EXCLUDED.email, updated_at = now()`,
		userID, email,
	)
	return err
}

// updateUserContactAccountLink fills in accountID and accountNumber for
// an existing user_contacts row. It is deliberately UPDATE-only, not an
// upsert: a row must already exist (created by upsertUserContactEmail
// from that user's UserActivated event) before an account can be attached
// to it — email is NOT NULL, and AccountCreated doesn't carry one. The
// returned bool reports whether a row was actually updated, so the caller
// can tell "linked" apart from "the UserActivated side hasn't landed yet".
//
// The NULLIF is there so an event without an account number stores NULL
// rather than an empty string: the two must not become distinguishable
// states meaning the same thing, since every reader of this column treats
// absence as "omit the account line from the email".
func updateUserContactAccountLink(ctx context.Context, pool *pgxpool.Pool, userID, accountID, accountNumber string) (bool, error) {
	tag, err := pool.Exec(ctx,
		`UPDATE user_contacts
		 SET account_id = $1, account_number = NULLIF($2, ''), updated_at = now()
		 WHERE user_id = $3`,
		accountID, accountNumber, userID,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// getContactByAccountID resolves a transfer event's sender_account_id or
// recipient_account_id — the only identity a Transfer* event carries —
// to someone we can mail. account_id is UNIQUE, so there is at most one
// row.
//
// COALESCE rather than a *string: every caller's degrade for "no account
// number on file" is "omit that line", and "" expresses that without
// pointer handling at four call sites.
func getContactByAccountID(ctx context.Context, pool *pgxpool.Pool, accountID string) (contact, bool, error) {
	var c contact
	err := pool.QueryRow(ctx,
		"SELECT user_id, email, COALESCE(account_number, '') FROM user_contacts WHERE account_id = $1",
		accountID,
	).Scan(&c.UserID, &c.Email, &c.AccountNumber)
	if errors.Is(err, pgx.ErrNoRows) {
		return contact{}, false, nil
	}
	if err != nil {
		return contact{}, false, err
	}
	return c, true, nil
}

// isEventProcessed reports whether eventID is already recorded in
// notifications_processed_events — idempotency fast-path, mirrors
// accounts-svc's own isEventProcessed. Named with a prefix, not the bare
// "processed_events" accounts-svc already uses, since both tables live in
// the same shared "neobank" Postgres database — see the migration's doc
// comment.
func isEventProcessed(ctx context.Context, pool *pgxpool.Pool, eventID string) (bool, error) {
	var exists bool
	err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM notifications_processed_events WHERE event_id = $1)",
		eventID,
	).Scan(&exists)
	return exists, err
}

// markEventProcessed records eventID as processed with the given status.
// ON CONFLICT DO NOTHING is cheap insurance against a future multi-replica
// deployment turning a duplicate bookkeeping write into a crash instead of
// a harmless no-op — same reasoning as accounts-svc's markEventProcessed.
//
// Used only by the two projection handlers. Never call it after
// claimEvent — see finishEvent for why that would be a silent disaster.
func markEventProcessed(ctx context.Context, pool *pgxpool.Pool, eventID, status string) error {
	_, err := pool.Exec(ctx,
		"INSERT INTO notifications_processed_events (event_id, status) VALUES ($1, $2) ON CONFLICT (event_id) DO NOTHING",
		eventID, status,
	)
	return err
}

// claimEvent is the barrier standing between a redelivered event and a
// duplicate email. It reports whether the caller now owns this event and
// should do the work. Pair every true with exactly one finishEvent.
//
// The statement is one round trip and atomic, and both properties matter:
//
//	INSERT ... VALUES ($1, 'processing')
//	ON CONFLICT (event_id) DO UPDATE SET status = <table>.status
//	RETURNING status
//
// The DO UPDATE is a deliberate no-op — it assigns the row its own
// existing value. Its only job is to make RETURNING fire on the conflict
// path: ON CONFLICT DO NOTHING returns zero rows, so a plain upsert
// cannot report what was already there. With DO UPDATE, RETURNING yields
// the post-update row, which equals the pre-existing one, so:
//
//	processing — either we just inserted it, or a previous attempt
//	             claimed it and never finished. Both mean "go".
//	sent       — the emails went out. Skip.
//	skipped    — no email by design. Skip.
//
// Why not the isEventProcessed + markEventProcessed pair the projection
// handlers use: their side effect is an upsert, so the read-then-write
// race between the two calls is harmless — doing it twice is doing it
// once. This handler's side effect is an email, which does not deduplicate
// itself, so the check and the claim have to be the same statement.
//
// Treating 'processing' as "go" is this sprint's chosen policy, not a
// fallback. Sending an email cannot join a database transaction — an
// external side effect does not roll back — so exactly-once is
// unavailable here in principle, and only the DIRECTION of the error is
// ours to pick. A crash between smtp.SendMail returning and finishEvent
// committing leaves a row that honestly says "we don't know whether that
// message was delivered"; retrying it risks a duplicate, skipping it
// risks silence. For notifications about money a second "you received
// 25.00 EUR" is a mild annoyance, while a missing one is a customer who
// doesn't know their money arrived. Duplicate over loss.
//
// One property this does NOT have, worth knowing before someone scales
// this service out: it does not serialize two concurrent workers. Worker
// A's claim auto-commits immediately, so worker B conflicting a
// millisecond later reads 'processing' and proceeds too. Unreachable
// today — one replica, and Kafka gives a group one consumer per partition
// — but it is a property of the 'processing' → go policy, not of the SQL.
func claimEvent(ctx context.Context, pool *pgxpool.Pool, eventID string) (bool, error) {
	var status string
	err := pool.QueryRow(ctx,
		`INSERT INTO notifications_processed_events (event_id, status)
		 VALUES ($1, 'processing')
		 ON CONFLICT (event_id) DO UPDATE SET status = notifications_processed_events.status
		 RETURNING status`,
		eventID,
	).Scan(&status)
	if err != nil {
		return false, err
	}
	return status == eventStatusProcessing, nil
}

// finishEvent moves a claimed row from 'processing' to its terminal
// status.
//
// It is an UPDATE, and it must not be replaced by markEventProcessed:
// after claimEvent the row already exists, so that function's ON CONFLICT
// DO NOTHING would silently do exactly that — leaving the row at
// 'processing' forever and turning every future redelivery into a fresh
// set of emails. That substitution is the single most likely way to break
// this quietly, which is why the two functions are kept distinct rather
// than "unified".
//
// processed_at is restamped so it means "when this finished", which makes
//
//	SELECT * FROM notifications_processed_events
//	 WHERE status = 'processing' AND processed_at < now() - interval '5 minutes'
//
// a usable "did we die mid-send?" query.
func finishEvent(ctx context.Context, pool *pgxpool.Pool, eventID, status string) error {
	_, err := pool.Exec(ctx,
		"UPDATE notifications_processed_events SET status = $1, processed_at = now() WHERE event_id = $2",
		status, eventID,
	)
	return err
}
