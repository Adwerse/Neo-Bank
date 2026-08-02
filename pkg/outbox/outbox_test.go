package outbox

import (
	"context"
	crand "crypto/rand"
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// newTestPool connects to the postgres instance from docker-compose.yml
// via DATABASE_URL, skipping the test if it isn't set — matching every
// service's own test convention for tests that exercise real SQL.
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

func randomHexForTest(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := crand.Read(b); err != nil {
		t.Fatalf("generate random suffix: %v", err)
	}
	return fmt.Sprintf("%x", b)
}

// newTestOutboxTable creates a table with the same shape every service's
// own outbox migration uses, under a random per-test name so tests never
// collide with each other or with any real service's table. This
// package owns no migrations of its own — each service does — so its
// tests build and tear down their own throwaway table.
func newTestOutboxTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	table := "outbox_pkg_test_" + randomHexForTest(t)
	_, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %s (
			id BIGSERIAL PRIMARY KEY,
			event_id UUID NOT NULL UNIQUE,
			event_type TEXT NOT NULL,
			partition_key TEXT NOT NULL,
			payload BYTEA NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			published_at TIMESTAMPTZ
		)`, table))
	if err != nil {
		t.Fatalf("create test outbox table: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), fmt.Sprintf("DROP TABLE IF EXISTS %s", table)); err != nil {
			t.Logf("cleanup: drop test outbox table %s: %v", table, err)
		}
	})
	return table
}

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestGenerateEventID(t *testing.T) {
	id, err := GenerateEventID()
	if err != nil {
		t.Fatalf("GenerateEventID: unexpected error: %v", err)
	}
	if !uuidV4Pattern.MatchString(id) {
		t.Errorf("GenerateEventID() = %q, want a UUIDv4-shaped string", id)
	}

	id2, err := GenerateEventID()
	if err != nil {
		t.Fatalf("GenerateEventID: unexpected error: %v", err)
	}
	if id == id2 {
		t.Error("two consecutive GenerateEventID() calls returned the same value")
	}
}

func TestInsertEvent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	table := newTestOutboxTable(t, ctx, pool)

	eventID, err := GenerateEventID()
	if err != nil {
		t.Fatalf("GenerateEventID: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := InsertEvent(ctx, tx, table, eventID, "SomethingHappened", "partition-1", []byte("payload-bytes")); err != nil {
		t.Fatalf("InsertEvent: unexpected error: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	var gotEventType, gotPartitionKey string
	var gotPayload []byte
	var gotPublishedAt *string
	err = pool.QueryRow(ctx,
		fmt.Sprintf("SELECT event_type, partition_key, payload, published_at FROM %s WHERE event_id = $1", table),
		eventID,
	).Scan(&gotEventType, &gotPartitionKey, &gotPayload, &gotPublishedAt)
	if err != nil {
		t.Fatalf("read back inserted row: %v", err)
	}
	if gotEventType != "SomethingHappened" {
		t.Errorf("event_type = %q, want %q", gotEventType, "SomethingHappened")
	}
	if gotPartitionKey != "partition-1" {
		t.Errorf("partition_key = %q, want %q", gotPartitionKey, "partition-1")
	}
	if string(gotPayload) != "payload-bytes" {
		t.Errorf("payload = %q, want %q", gotPayload, "payload-bytes")
	}
	if gotPublishedAt != nil {
		t.Errorf("published_at = %v, want nil", gotPublishedAt)
	}
}

// TestInsertEvent_RollsBackWithCaller proves InsertEvent shares its
// caller's transaction rather than being independently atomic: if the
// caller's transaction rolls back (for any reason, simulated here by
// simply never committing), the event row must not persist either — the
// whole point of the outbox pattern is that the business write and its
// event are one all-or-nothing unit.
func TestInsertEvent_RollsBackWithCaller(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	table := newTestOutboxTable(t, ctx, pool)

	eventID, err := GenerateEventID()
	if err != nil {
		t.Fatalf("GenerateEventID: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := InsertEvent(ctx, tx, table, eventID, "SomethingHappened", "partition-1", []byte("payload-bytes")); err != nil {
		t.Fatalf("InsertEvent: unexpected error: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE event_id = $1", table), eventID).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 0 {
		t.Errorf("rows for event_id=%s after rollback = %d, want 0", eventID, count)
	}
}
