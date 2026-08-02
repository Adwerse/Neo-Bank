package main

import (
	"context"
	crand "crypto/rand"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// newTestPool follows the convention pkg/outbox's tests established: DB
// tests skip rather than fail when there's no database to talk to, so
// `go test ./...` stays useful without docker compose running.
//
// These tests run against the real notifications-svc tables (created by
// this service's migrations), not throwaway copies — claimEvent's whole
// point is the ON CONFLICT behaviour of the actual primary key and CHECK
// constraint, which a hand-rolled test table would only approximate. Every
// test therefore uses randomly generated event_ids and cleans up after
// itself.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set; skipping test that requires a live postgres (see docker-compose.yml)")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// randomUUID builds a v4 UUID without pulling in a dependency, matching
// pkg/outbox.GenerateEventID's approach.
func randomUUID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		t.Fatalf("generate uuid: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func newTestEventID(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	eventID := randomUUID(t)
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM notifications_processed_events WHERE event_id = $1", eventID); err != nil {
			t.Logf("cleanup: delete test event %s: %v", eventID, err)
		}
	})
	return eventID
}

func eventStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, eventID string) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(ctx, "SELECT status FROM notifications_processed_events WHERE event_id = $1", eventID).Scan(&status); err != nil {
		t.Fatalf("read status for %s: %v", eventID, err)
	}
	return status
}

// TestClaimEvent_FirstClaimWins is the happy path: a fresh event is
// claimed, and the row it leaves behind says so.
func TestClaimEvent_FirstClaimWins(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	eventID := newTestEventID(t, ctx, pool)

	claimed, err := claimEvent(ctx, pool, eventID)
	if err != nil {
		t.Fatalf("claimEvent: %v", err)
	}
	if !claimed {
		t.Fatal("claimEvent on a fresh event returned false, want true")
	}
	if got := eventStatus(t, ctx, pool, eventID); got != eventStatusProcessing {
		t.Errorf("status after claim = %q, want %q", got, eventStatusProcessing)
	}
}

// TestClaimEvent_ReclaimsAfterCrash asserts the sprint's stated policy
// rather than assuming it: a row stuck at 'processing' means "we don't
// know whether the email went out", and this service chooses to send
// again (risking a duplicate) over staying silent (risking a customer
// who never learns their money moved). If someone later flips this to
// return false, that is a deliberate change of policy and this test is
// where they have to say so.
func TestClaimEvent_ReclaimsAfterCrash(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	eventID := newTestEventID(t, ctx, pool)

	if _, err := claimEvent(ctx, pool, eventID); err != nil {
		t.Fatalf("first claimEvent: %v", err)
	}
	// No finishEvent — exactly the state a crash between sending and
	// bookkeeping leaves behind.
	claimed, err := claimEvent(ctx, pool, eventID)
	if err != nil {
		t.Fatalf("second claimEvent: %v", err)
	}
	if !claimed {
		t.Error("claimEvent on a row left at 'processing' returned false, want true (duplicate over loss)")
	}
}

// TestClaimEvent_SkipsFinishedEvents is the redelivery path the DoD tests
// end to end: once finished, an event is never worked again.
func TestClaimEvent_SkipsFinishedEvents(t *testing.T) {
	for _, status := range []string{eventStatusSent, eventStatusSkipped} {
		t.Run(status, func(t *testing.T) {
			pool := newTestPool(t)
			ctx := context.Background()
			eventID := newTestEventID(t, ctx, pool)

			if _, err := claimEvent(ctx, pool, eventID); err != nil {
				t.Fatalf("claimEvent: %v", err)
			}
			if err := finishEvent(ctx, pool, eventID, status); err != nil {
				t.Fatalf("finishEvent: %v", err)
			}

			claimed, err := claimEvent(ctx, pool, eventID)
			if err != nil {
				t.Fatalf("claimEvent after finish: %v", err)
			}
			if claimed {
				t.Errorf("claimEvent on a %q event returned true, want false — this is what stops a redelivery from re-sending", status)
			}
			if got := eventStatus(t, ctx, pool, eventID); got != status {
				t.Errorf("status = %q after a rejected re-claim, want %q unchanged", got, status)
			}
		})
	}
}

// TestClaimEvent_LeavesExactlyOneRow — the DoD's "still one row" check,
// asserted directly rather than only through psql.
func TestClaimEvent_LeavesExactlyOneRow(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	eventID := newTestEventID(t, ctx, pool)

	for i := 0; i < 3; i++ {
		if _, err := claimEvent(ctx, pool, eventID); err != nil {
			t.Fatalf("claimEvent #%d: %v", i, err)
		}
	}
	if err := finishEvent(ctx, pool, eventID, eventStatusSent); err != nil {
		t.Fatalf("finishEvent: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM notifications_processed_events WHERE event_id = $1", eventID).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 1 {
		t.Errorf("got %d rows for one event_id, want 1", count)
	}
}

// TestGetContactByAccountID covers the lookup every transfer email
// depends on, including the NULL account_number that rows linked before
// migration 000003 still carry.
func TestGetContactByAccountID(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	userID := randomUUID(t)
	accountID := randomUUID(t)
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM user_contacts WHERE user_id = $1", userID); err != nil {
			t.Logf("cleanup: delete test contact %s: %v", userID, err)
		}
	})

	if _, err := pool.Exec(ctx,
		"INSERT INTO user_contacts (user_id, email) VALUES ($1, $2)",
		userID, "contact-test@example.com",
	); err != nil {
		t.Fatalf("seed user_contacts: %v", err)
	}

	if _, found, err := getContactByAccountID(ctx, pool, randomUUID(t)); err != nil {
		t.Fatalf("lookup of an unknown account: %v", err)
	} else if found {
		t.Error("getContactByAccountID found a contact for an unknown account id")
	}

	linked, err := updateUserContactAccountLink(ctx, pool, userID, accountID, "")
	if err != nil {
		t.Fatalf("link account without a number: %v", err)
	}
	if !linked {
		t.Fatal("updateUserContactAccountLink reported no row updated")
	}
	c, found, err := getContactByAccountID(ctx, pool, accountID)
	if err != nil {
		t.Fatalf("lookup after linking: %v", err)
	}
	if !found {
		t.Fatal("getContactByAccountID did not find the linked contact")
	}
	if c.Email != "contact-test@example.com" {
		t.Errorf("Email = %q, want %q", c.Email, "contact-test@example.com")
	}
	if c.AccountNumber != "" {
		t.Errorf("AccountNumber = %q, want \"\" — a NULL column must read as the empty string the templates treat as 'omit the line'", c.AccountNumber)
	}

	if _, err := updateUserContactAccountLink(ctx, pool, userID, accountID, "NB0012345678"); err != nil {
		t.Fatalf("link account with a number: %v", err)
	}
	c, _, err = getContactByAccountID(ctx, pool, accountID)
	if err != nil {
		t.Fatalf("lookup after backfilling the number: %v", err)
	}
	if c.AccountNumber != "NB0012345678" {
		t.Errorf("AccountNumber = %q, want %q", c.AccountNumber, "NB0012345678")
	}
}
