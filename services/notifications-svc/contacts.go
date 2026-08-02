package main

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// upsertUserContactEmail records (or updates) userID's email in the
// user_contacts projection, from a UserActivated event. ON CONFLICT only
// touches email/updated_at — it must never clobber account_id, which is
// filled in separately (and possibly later) by updateUserContactAccountID
// from an AccountCreated event.
func upsertUserContactEmail(ctx context.Context, pool *pgxpool.Pool, userID, email string) error {
	_, err := pool.Exec(ctx,
		`INSERT INTO user_contacts (user_id, email) VALUES ($1, $2)
		 ON CONFLICT (user_id) DO UPDATE SET email = EXCLUDED.email, updated_at = now()`,
		userID, email,
	)
	return err
}

// updateUserContactAccountID fills in accountID for an existing
// user_contacts row. It is deliberately UPDATE-only, not an upsert: a row
// must already exist (created by upsertUserContactEmail from that user's
// UserActivated event) before an account_id can be attached to it — email
// is NOT NULL, and AccountCreated doesn't carry one. The returned bool
// reports whether a row was actually updated, so the caller can tell
// "linked" apart from "the UserActivated side hasn't landed yet".
func updateUserContactAccountID(ctx context.Context, pool *pgxpool.Pool, userID, accountID string) (bool, error) {
	tag, err := pool.Exec(ctx,
		"UPDATE user_contacts SET account_id = $1, updated_at = now() WHERE user_id = $2",
		accountID, userID,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
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
func markEventProcessed(ctx context.Context, pool *pgxpool.Pool, eventID, status string) error {
	_, err := pool.Exec(ctx,
		"INSERT INTO notifications_processed_events (event_id, status) VALUES ($1, $2) ON CONFLICT (event_id) DO NOTHING",
		eventID, status,
	)
	return err
}
