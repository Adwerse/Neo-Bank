package outbox

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultCleanupInterval is how often RunCleanupWorker checks for
// published rows old enough to delete, absent a caller-specific reason
// to choose otherwise.
const DefaultCleanupInterval = 1 * time.Hour

// RunCleanupWorker periodically deletes rows from table that have been
// published for longer than retention. Published rows aren't deleted
// right after publishing — they're kept around deliberately, so that
// debugging "did this event actually go out" has somewhere to look; only
// rows old enough that nobody's going to ask that question anymore get
// removed. An unpublished row is never a cleanup candidate no matter how
// old — see CleanupPublished's WHERE clause — since deleting one would
// be exactly the silent loss the outbox pattern exists to prevent.
func RunCleanupWorker(ctx context.Context, pool *pgxpool.Pool, table string, retention, interval time.Duration, logPrefix string) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			deleted, err := CleanupPublished(ctx, pool, table, retention)
			if err != nil {
				log.Printf("%s: outbox cleanup: %v", logPrefix, err)
				continue
			}
			if deleted > 0 {
				log.Printf("%s: outbox cleanup: deleted %d published row(s) older than %s", logPrefix, deleted, retention)
			}
		}
	}
}

// CleanupPublished deletes published rows from table older than
// retention and reports how many rows were removed.
func CleanupPublished(ctx context.Context, pool *pgxpool.Pool, table string, retention time.Duration) (int64, error) {
	tag, err := pool.Exec(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE published_at IS NOT NULL AND published_at < now() - make_interval(secs => $1)`, table),
		retention.Seconds(),
	)
	if err != nil {
		return 0, fmt.Errorf("delete published outbox rows from %s: %w", table, err)
	}
	return tag.RowsAffected(), nil
}
