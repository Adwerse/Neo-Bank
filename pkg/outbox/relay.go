package outbox

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
)

// DefaultBatchSize is how many unpublished rows RelayBatch claims per
// pass, absent a caller-specific reason to choose otherwise.
const DefaultBatchSize = 100

// DefaultPublishTimeout bounds a single WriteMessages call, so a hung
// broker connection can't stall a relay pass (and hold its row locks)
// indefinitely.
const DefaultPublishTimeout = 5 * time.Second

// KafkaMessageWriter is the narrow slice of *kafka.Writer's API the relay
// needs, narrowed to an interface so callers' tests can substitute a
// fake instead of a live broker. *kafka.Writer satisfies this implicitly.
type KafkaMessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

type relayRow struct {
	ID           int64
	EventID      string
	EventType    string
	PartitionKey string
	Payload      []byte
}

// RunRelay is the process that closes the dual-write gap InsertEvent's
// callers exist to avoid: on a ticker, it reads rows from table that
// nothing has published yet and hands them to Kafka. Runs for the
// lifetime of the process — no graceful shutdown, matching every other
// background loop in this repo.
func RunRelay(ctx context.Context, pool *pgxpool.Pool, table string, writer KafkaMessageWriter, interval time.Duration, batchSize int, logPrefix string) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			RelayBatch(ctx, pool, table, writer, batchSize, logPrefix)
		}
	}
}

// RelayBatch claims up to batchSize unpublished rows from table with
// SELECT ... FOR UPDATE SKIP LOCKED, publishes each to Kafka, and marks
// it published — all within one transaction that holds the row locks for
// as long as this batch takes to process. SKIP LOCKED is what makes
// running more than one instance of a service safe: two instances
// polling at the same time each claim their own batch and silently skip
// whatever the other is already holding, instead of blocking on (or
// double-publishing) the same rows.
//
// Publish happens BEFORE the published_at UPDATE, not after, and that
// order is deliberate: if this process crashes between the two, the row
// is still published_at IS NULL, so the next tick (by this instance or
// another) publishes it again. That's a duplicate, and the consumer side
// is expected to dedupe by event_id — but the reverse order (mark
// published, then actually publish) would be at-most-once: a crash there
// leaves a row marked published that Kafka never received, and that loss
// is silent forever, since nothing about the row looks wrong afterward.
// A harmless duplicate beats an invisible loss, so this relay is
// deliberately at-least-once.
//
// Exported separately from RunRelay so callers' tests can invoke a
// single pass directly against a fake KafkaMessageWriter.
func RelayBatch(ctx context.Context, pool *pgxpool.Pool, table string, writer KafkaMessageWriter, batchSize int, logPrefix string) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		log.Printf("%s: outbox relay: begin tx: %v", logPrefix, err)
		return
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx,
		fmt.Sprintf(`SELECT id, event_id, event_type, partition_key, payload
		 FROM %s
		 WHERE published_at IS NULL
		 ORDER BY id
		 LIMIT $1
		 FOR UPDATE SKIP LOCKED`, table),
		batchSize,
	)
	if err != nil {
		log.Printf("%s: outbox relay: claim batch: %v", logPrefix, err)
		return
	}
	var batch []relayRow
	for rows.Next() {
		var r relayRow
		if err := rows.Scan(&r.ID, &r.EventID, &r.EventType, &r.PartitionKey, &r.Payload); err != nil {
			rows.Close()
			log.Printf("%s: outbox relay: scan claimed row: %v", logPrefix, err)
			return
		}
		batch = append(batch, r)
	}
	if err := rows.Err(); err != nil {
		log.Printf("%s: outbox relay: read claimed batch: %v", logPrefix, err)
		return
	}

	// Stop at the first publish/update failure rather than skipping past
	// it: if Kafka is unreachable, every remaining row would fail the
	// same way, so there's nothing to gain from trying them (their row
	// locks release harmlessly on this function's return — deferred
	// Rollback — leaving them for the next tick to retry). Rows already
	// published and marked earlier in this same loop still get committed
	// below: the break only stops the batch from progressing further, it
	// doesn't undo work this call already completed.
	for _, r := range batch {
		writeCtx, cancel := context.WithTimeout(ctx, DefaultPublishTimeout)
		err := writer.WriteMessages(writeCtx, kafka.Message{
			Key:   []byte(r.PartitionKey),
			Value: r.Payload,
		})
		cancel()
		if err != nil {
			log.Printf("%s: outbox relay: publish event_id=%s type=%s: %v (will retry next tick)", logPrefix, r.EventID, r.EventType, err)
			break
		}

		if _, err := tx.Exec(ctx, fmt.Sprintf("UPDATE %s SET published_at = now() WHERE id = $1", table), r.ID); err != nil {
			log.Printf("%s: outbox relay: mark published event_id=%s: %v", logPrefix, r.EventID, err)
			break
		}
	}

	if err := tx.Commit(ctx); err != nil {
		log.Printf("%s: outbox relay: commit tx: %v", logPrefix, err)
	}
}
