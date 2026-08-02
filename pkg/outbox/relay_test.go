package outbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
)

// fakeKafkaWriter implements KafkaMessageWriter without a live Kafka
// broker, recording every message it's asked to write. failKey, if set,
// makes WriteMessages fail whenever it sees a message with that key,
// regardless of call order — a positional "fail on the Nth call" fake
// would be fragile here, since a batch can contain rows in an order this
// test doesn't fully control.
type fakeKafkaWriter struct {
	mu       sync.Mutex
	messages []kafka.Message
	failKey  string
}

func (f *fakeKafkaWriter) WriteMessages(ctx context.Context, msgs ...kafka.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range msgs {
		if f.failKey != "" && string(m.Key) == f.failKey {
			return errors.New("simulated kafka write failure")
		}
	}
	f.messages = append(f.messages, msgs...)
	return nil
}

func containsMessage(messages []kafka.Message, key string, value []byte) bool {
	for _, m := range messages {
		if string(m.Key) == key && bytes.Equal(m.Value, value) {
			return true
		}
	}
	return false
}

func insertRowForTest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, eventID, eventType, partitionKey string, payload []byte) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx,
		fmt.Sprintf(`INSERT INTO %s (event_id, event_type, partition_key, payload) VALUES ($1, $2, $3, $4) RETURNING id`, table),
		eventID, eventType, partitionKey, payload,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert row: %v", err)
	}
	return id
}

func getPublishedAt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string, id int64) *time.Time {
	t.Helper()
	var publishedAt *time.Time
	if err := pool.QueryRow(ctx, fmt.Sprintf("SELECT published_at FROM %s WHERE id = $1", table), id).Scan(&publishedAt); err != nil {
		t.Fatalf("get published_at id=%d: %v", id, err)
	}
	return publishedAt
}

func randomEventID(t *testing.T) string {
	t.Helper()
	id, err := GenerateEventID()
	if err != nil {
		t.Fatalf("GenerateEventID: %v", err)
	}
	return id
}

func TestRelayBatch_PublishesAndMarksPublished(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	table := newTestOutboxTable(t, ctx, pool)

	partitionKey := randomHexForTest(t)
	id1 := insertRowForTest(t, ctx, pool, table, randomEventID(t), "Thing", partitionKey, []byte("payload-1"))
	id2 := insertRowForTest(t, ctx, pool, table, randomEventID(t), "Thing", partitionKey, []byte("payload-2"))

	writer := &fakeKafkaWriter{}
	RelayBatch(ctx, pool, table, writer, DefaultBatchSize, "test")

	if !containsMessage(writer.messages, partitionKey, []byte("payload-1")) {
		t.Error("relay did not publish payload-1")
	}
	if !containsMessage(writer.messages, partitionKey, []byte("payload-2")) {
		t.Error("relay did not publish payload-2")
	}
	if getPublishedAt(t, ctx, pool, table, id1) == nil {
		t.Error("row 1 published_at is nil, want set")
	}
	if getPublishedAt(t, ctx, pool, table, id2) == nil {
		t.Error("row 2 published_at is nil, want set")
	}
}

func TestRelayBatch_StopsOnPublishFailureButKeepsEarlierProgress(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	table := newTestOutboxTable(t, ctx, pool)

	partitionKey := randomHexForTest(t)
	poisonKey := randomHexForTest(t)
	id1 := insertRowForTest(t, ctx, pool, table, randomEventID(t), "Thing", partitionKey, []byte("row-1"))
	id2 := insertRowForTest(t, ctx, pool, table, randomEventID(t), "Thing", poisonKey, []byte("row-2"))
	id3 := insertRowForTest(t, ctx, pool, table, randomEventID(t), "Thing", partitionKey, []byte("row-3"))

	writer := &fakeKafkaWriter{failKey: poisonKey}
	RelayBatch(ctx, pool, table, writer, DefaultBatchSize, "test")

	if getPublishedAt(t, ctx, pool, table, id1) == nil {
		t.Error("row 1 (before the failure) published_at is nil, want set — earlier progress in the batch must survive a later failure")
	}
	if getPublishedAt(t, ctx, pool, table, id2) != nil {
		t.Error("row 2 (the failing publish) published_at is set, want nil")
	}
	if getPublishedAt(t, ctx, pool, table, id3) != nil {
		t.Error("row 3 (after the failure) published_at is set, want nil — the relay must stop at the first failure, not skip past it")
	}
}

func TestRelayBatch_SkipsAlreadyPublishedRows(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	table := newTestOutboxTable(t, ctx, pool)

	partitionKey := randomHexForTest(t)
	publishedID := insertRowForTest(t, ctx, pool, table, randomEventID(t), "Thing", partitionKey, []byte("already-published"))
	if _, err := pool.Exec(ctx, fmt.Sprintf("UPDATE %s SET published_at = now() WHERE id = $1", table), publishedID); err != nil {
		t.Fatalf("backdate published_at: %v", err)
	}
	unpublishedID := insertRowForTest(t, ctx, pool, table, randomEventID(t), "Thing", partitionKey, []byte("not-yet-published"))

	writer := &fakeKafkaWriter{}
	RelayBatch(ctx, pool, table, writer, DefaultBatchSize, "test")

	if containsMessage(writer.messages, partitionKey, []byte("already-published")) {
		t.Error("relay re-published an already-published row")
	}
	if !containsMessage(writer.messages, partitionKey, []byte("not-yet-published")) {
		t.Error("relay did not publish the unpublished row")
	}
	if getPublishedAt(t, ctx, pool, table, unpublishedID) == nil {
		t.Error("unpublished row's published_at is nil after relay, want set")
	}
}

// TestRelayBatch_SkipsLockedRows proves the reason FOR UPDATE SKIP LOCKED
// is in the query at all: with a second, independent transaction already
// holding a row lock (simulating another instance's relay having claimed
// it first), this call must skip that row rather than block waiting for
// the lock or publish it a second time.
func TestRelayBatch_SkipsLockedRows(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	table := newTestOutboxTable(t, ctx, pool)

	partitionKey := randomHexForTest(t)
	lockedID := insertRowForTest(t, ctx, pool, table, randomEventID(t), "Thing", partitionKey, []byte("locked-row"))
	freeID := insertRowForTest(t, ctx, pool, table, randomEventID(t), "Thing", partitionKey, []byte("free-row"))

	holderTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	defer holderTx.Rollback(ctx)
	if _, err := holderTx.Exec(ctx, fmt.Sprintf("SELECT id FROM %s WHERE id = $1 FOR UPDATE", table), lockedID); err != nil {
		t.Fatalf("lock row: %v", err)
	}

	writer := &fakeKafkaWriter{}
	RelayBatch(ctx, pool, table, writer, DefaultBatchSize, "test")

	if containsMessage(writer.messages, partitionKey, []byte("locked-row")) {
		t.Error("relay published a row locked by another transaction — SKIP LOCKED should have skipped it")
	}
	if !containsMessage(writer.messages, partitionKey, []byte("free-row")) {
		t.Error("relay did not publish the unlocked row")
	}
	if getPublishedAt(t, ctx, pool, table, lockedID) != nil {
		t.Error("locked row's published_at is set, want nil (must not be touched while locked)")
	}
	if getPublishedAt(t, ctx, pool, table, freeID) == nil {
		t.Error("unlocked row's published_at is nil, want set")
	}
}
