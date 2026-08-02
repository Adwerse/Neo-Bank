package outbox

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func backdatePublishedAt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string, id int64, age time.Duration) {
	t.Helper()
	if _, err := pool.Exec(ctx, fmt.Sprintf("UPDATE %s SET published_at = $1 WHERE id = $2", table), time.Now().Add(-age), id); err != nil {
		t.Fatalf("backdate published_at: %v", err)
	}
}

func rowExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string, id int64) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, fmt.Sprintf("SELECT EXISTS(SELECT 1 FROM %s WHERE id = $1)", table), id).Scan(&exists); err != nil {
		t.Fatalf("check row exists id=%d: %v", id, err)
	}
	return exists
}

func TestCleanupPublished(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	table := newTestOutboxTable(t, ctx, pool)

	partitionKey := randomHexForTest(t)

	oldPublished := insertRowForTest(t, ctx, pool, table, randomEventID(t), "Thing", partitionKey, []byte("old"))
	backdatePublishedAt(t, ctx, pool, table, oldPublished, 48*time.Hour)

	recentPublished := insertRowForTest(t, ctx, pool, table, randomEventID(t), "Thing", partitionKey, []byte("recent"))
	backdatePublishedAt(t, ctx, pool, table, recentPublished, 1*time.Minute)

	unpublished := insertRowForTest(t, ctx, pool, table, randomEventID(t), "Thing", partitionKey, []byte("unpublished"))

	deleted, err := CleanupPublished(ctx, pool, table, 24*time.Hour)
	if err != nil {
		t.Fatalf("CleanupPublished: unexpected error: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}

	if rowExists(t, ctx, pool, table, oldPublished) {
		t.Error("old published row still exists, want deleted")
	}
	if !rowExists(t, ctx, pool, table, recentPublished) {
		t.Error("recent published row was deleted, want kept")
	}
	if !rowExists(t, ctx, pool, table, unpublished) {
		t.Error("unpublished row was deleted, want kept regardless of age")
	}
}
