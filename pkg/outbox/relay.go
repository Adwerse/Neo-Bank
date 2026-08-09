package outbox

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel/attribute"

	"neobank/pkg/tracing"
)

// DefaultBatchSize is how many unpublished rows RelayBatch claims per
// pass, absent a caller-specific reason to choose otherwise.
const DefaultBatchSize = 100

// DefaultPublishTimeout bounds a single WriteMessages call, so a hung
// broker connection can't stall a relay pass (and hold its row locks)
// indefinitely.
const DefaultPublishTimeout = 5 * time.Second

// HeaderEventType is the Kafka message header key every relayed event
// carries; its value is the outbox row's event_type verbatim
// ("TransferCompleted", "UserActivated", ...).
//
// It exists because protobuf's wire format is not self-describing and
// transfer.events is the first topic in this repo carrying more than one
// message type. TransferCompleted/TransferFailed/TransferRejected share
// field numbers AND field types for 1-5, and their field 6 is a string in
// all three (ledger_transaction_id / reason / triggered_rule) — so
// proto.Unmarshal of any one of them into any other SUCCEEDS, silently,
// depositing a failure reason into LedgerTransactionId. Without a
// discriminator outside the payload, a consumer of that topic cannot know
// what it is holding.
//
// The relay is the right place to stamp it: event_type is already a
// column on every outbox row (it was previously read only to be logged),
// so producers need no change and the header can never disagree with what
// was written in the same transaction as the state change. Adding a
// header is additive on the wire — existing consumers that don't look at
// it are unaffected.
const HeaderEventType = "event_type"

// KafkaMessageWriter is the narrow slice of *kafka.Writer's API the relay
// needs, narrowed to an interface so callers' tests can substitute a
// fake instead of a live broker. *kafka.Writer satisfies this implicitly.
type KafkaMessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// tracerScope names the instrumentation scope for spans this package
// creates. Distinct from the service name (which lives in the resource),
// so Jaeger can tell "a span from the outbox relay" apart from "a span
// transfers-svc wrote by hand".
const tracerScope = "neobank/pkg/outbox"

type relayRow struct {
	ID           int64
	EventID      string
	EventType    string
	PartitionKey string
	Payload      []byte
	// TraceContext is the serialized trace of the request that produced
	// this event, written by InsertEvent in the same transaction. Empty
	// for rows written before the column existed or while tracing was
	// off — both normal.
	TraceContext string
	// CreatedAt is when the event was written, used to report how long it
	// waited for the relay as a span attribute.
	CreatedAt time.Time
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
		fmt.Sprintf(`SELECT id, event_id, event_type, partition_key, payload, COALESCE(trace_context::text, ''), created_at
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
		if err := rows.Scan(&r.ID, &r.EventID, &r.EventType, &r.PartitionKey, &r.Payload, &r.TraceContext, &r.CreatedAt); err != nil {
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
		// Each event gets its own span, and that span STARTS A NEW TRACE
		// linked back to the request that produced the event, rather than
		// continuing that request's trace as a child.
		//
		// The alternative — parenting this to the originating span — is
		// available (the context is right there in r.TraceContext) and is
		// the wrong choice. That HTTP request finished in tens of
		// milliseconds; this publish happens up to a second later, and
		// after a Kafka outage, minutes later. A parent span's duration is
		// supposed to contain its children, so parenting would draw a 50ms
		// bar with a child beginning three seconds after the bar ended,
		// and would corrupt every duration statistic computed over the
		// request span. A link states the true relationship — separate
		// work, caused by that trace — and Jaeger makes it navigable from
		// both ends. See README, "Связь спанов".
		publishCtx, span := tracing.StartLinkedRoot(ctx, tracerScope, "outbox publish "+r.EventType, r.TraceContext,
			attribute.String("messaging.system", "kafka"),
			attribute.String("messaging.operation", "publish"),
			attribute.String("neobank.outbox.table", table),
			attribute.String("neobank.event.id", r.EventID),
			attribute.String("neobank.event.type", r.EventType),
			// How long the event sat unpublished. This is the number the
			// outbox pattern trades for durability, and having it on the
			// span makes relay lag answerable without a query.
			attribute.Int64("neobank.outbox.lag_ms", time.Since(r.CreatedAt).Milliseconds()),
		)

		// Headers carry the DELIVERY trace (this span), not the original
		// request's. Consumers therefore continue this trace as ordinary
		// children — which is correct, because they really are caused by
		// this publish and follow it closely — while the link keeps the
		// path back to the originating request one click away.
		headers := []kafka.Header{{Key: HeaderEventType, Value: []byte(r.EventType)}}
		for k, v := range tracing.InjectMap(publishCtx) {
			headers = append(headers, kafka.Header{Key: k, Value: []byte(v)})
		}

		writeCtx, cancel := context.WithTimeout(publishCtx, DefaultPublishTimeout)
		err := writer.WriteMessages(writeCtx, kafka.Message{
			Key:     []byte(r.PartitionKey),
			Value:   r.Payload,
			Headers: headers,
		})
		cancel()
		if err != nil {
			log.Printf("%s: outbox relay: publish event_id=%s type=%s: %v (will retry next tick)", logPrefix, r.EventID, r.EventType, err)
			tracing.Fail(publishCtx, "outbox_publish_failed", err)
			span.End()
			break
		}

		if _, err := tx.Exec(publishCtx, fmt.Sprintf("UPDATE %s SET published_at = now() WHERE id = $1", table), r.ID); err != nil {
			log.Printf("%s: outbox relay: mark published event_id=%s: %v", logPrefix, r.EventID, err)
			tracing.Fail(publishCtx, "outbox_mark_published_failed", err)
			span.End()
			break
		}
		span.End()
	}

	if err := tx.Commit(ctx); err != nil {
		log.Printf("%s: outbox relay: commit tx: %v", logPrefix, err)
	}
}
