package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	"neobank/pkg/tracing"
	eventsv1 "neobank/proto/gen/go/events/v1"
)

// tracerScope names the gateway's instrumentation scope for hand-written
// spans.
const tracerScope = "neobank/gateway"

// traceContextFromMessage grafts the trace context the outbox relay
// injected into the message headers onto ctx, so a WebSocket push shows
// up inside the delivery trace of the event that triggered it instead of
// as an orphan. Duplicated in notifications-svc and accounts-svc rather
// than shared — the common thing here is the wire format, not the code.
func traceContextFromMessage(ctx context.Context, msg kafka.Message) context.Context {
	if len(msg.Headers) == 0 {
		return ctx
	}
	carrier := make(map[string]string, len(msg.Headers))
	for _, h := range msg.Headers {
		carrier[h.Key] = string(h.Value)
	}
	return tracing.ExtractMap(ctx, carrier)
}

const (
	defaultKafkaBrokers        = "kafka:9092"
	defaultAccountEventsTopic  = "account.events"
	defaultTransferEventsTopic = "transfer.events"
	defaultAccountCacheTTL     = time.Hour

	// fetchErrorBackoff keeps a failing FetchMessage from becoming a hot
	// spin — same constant and same reasoning as notifications-svc's.
	fetchErrorBackoff = time.Second
)

// eventTypeHeader duplicates pkg/outbox.HeaderEventType's literal rather
// than importing that module, for the same reason notifications-svc's
// copy does (services/notifications-svc/kafka.go): this is a pure
// consumer of transfer.events/account.events, owns no outbox table, and
// publishes nothing to either topic. Depending on the publishing-side
// library would invert the layering.
const eventTypeHeader = "event_type"

// The event_type values transfers-svc/accounts-svc write into their
// outbox rows, copied verbatim onto this header by the relay.
// DepositCredited shares transfer.events (and its outbox table) with the
// three transfer events — see README's "Gateway: Kafka → WebSocket" for
// why no separate deposit topic exists.
const (
	eventTypeAccountCreated    = "AccountCreated"
	eventTypeTransferCompleted = "TransferCompleted"
	eventTypeTransferFailed    = "TransferFailed"
	eventTypeTransferRejected  = "TransferRejected"
	eventTypeDepositCredited   = "DepositCredited"
)

// eventTypeOf reads the event_type header off msg, or "" if absent —
// identical logic to notifications-svc's helper of the same name, since
// both consume transfer.events and face the same wire-ambiguity problem
// (see pkg/outbox/relay.go's HeaderEventType doc comment).
func eventTypeOf(msg kafka.Message) string {
	for _, h := range msg.Headers {
		if h.Key == eventTypeHeader {
			return string(h.Value)
		}
	}
	return ""
}

// newInstanceConsumerGroup returns a consumer group id unique to this
// process, generated fresh on every call — never persisted, never
// derived solely from something stable like the hostname. That freshness
// is load-bearing: it's what makes every gateway instance read every
// event on transfer.events/account.events (the fix for the
// multi-instance problem — a user's WS connection lives on exactly one
// instance, so every instance must see every event and decide locally
// whether it has anyone to tell), instead of Kafka splitting partitions
// across instances the way a shared group would. It's also what makes
// kafka.LastOffset behave as "start from now" on every restart rather
// than "resume from whatever this host last committed."
func newInstanceConsumerGroup() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		// crypto/rand failing is effectively unrecoverable for this
		// process's other needs too; a timestamp-based fallback still
		// keeps the group unique enough for its one purpose here.
		return fmt.Sprintf("gateway-%s-%d", host, time.Now().UnixNano())
	}
	return fmt.Sprintf("gateway-%s-%s", host, hex.EncodeToString(suffix[:]))
}

func newGatewayKafkaReader(brokers []string, groupID, topic string, startOffset int64) *kafka.Reader {
	return kafka.NewReader(kafka.ReaderConfig{
		Brokers:     brokers,
		Topic:       topic,
		GroupID:     groupID,
		StartOffset: startOffset,
	})
}

// runAccountEventsConsumer backfills accountCache from account.events.
// kafka.FirstOffset (set by the caller) replays the whole compacted topic
// on this fresh, never-before-seen group — exactly the "rebuild a
// projection" case newProjectionReader documents in notifications-svc:
// the cache is local, in-memory state, so a freshly started instance has
// none of it yet and must read from the beginning to catch up.
func runAccountEventsConsumer(fetchCtx context.Context, reader *kafka.Reader, cache *accountCache) {
	const topic = "account.events"
	defer reader.Close()
	for {
		msg, err := reader.FetchMessage(fetchCtx)
		if err != nil {
			if fetchCtx.Err() != nil {
				log.Printf("gateway: %s: shutting down", topic)
				return
			}
			log.Printf("gateway: %s: failed to fetch message: %v", topic, err)
			time.Sleep(fetchErrorBackoff)
			continue
		}
		ctx, span := otel.Tracer(tracerScope).Start(
			traceContextFromMessage(context.Background(), msg),
			"gateway handle "+topic,
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(
				attribute.String("messaging.system", "kafka"),
				attribute.String("messaging.source.name", topic),
				attribute.Int64("messaging.kafka.offset", msg.Offset),
			),
		)

		if eventTypeOf(msg) != eventTypeAccountCreated {
			// account.events carries only AccountCreated today, but
			// forward-compat: skip and commit rather than crash on
			// something this instance doesn't understand yet.
			if err := reader.CommitMessages(ctx, msg); err != nil {
				log.Printf("gateway: %s: failed to commit offset at partition %d offset %d: %v", topic, msg.Partition, msg.Offset, err)
			}
			span.End()
			continue
		}

		var event eventsv1.AccountCreated
		if err := proto.Unmarshal(msg.Value, &event); err != nil {
			log.Printf("gateway: %s: failed to unmarshal AccountCreated at offset %d: %v", topic, msg.Offset, err)
			if err := reader.CommitMessages(ctx, msg); err != nil {
				log.Printf("gateway: %s: failed to commit offset at partition %d offset %d: %v", topic, msg.Partition, msg.Offset, err)
			}
			span.End()
			continue
		}

		cache.set(event.GetAccountId(), event.GetUserId())

		if err := reader.CommitMessages(ctx, msg); err != nil {
			log.Printf("gateway: %s: failed to commit offset at partition %d offset %d: %v", topic, msg.Partition, msg.Offset, err)
		}
		span.End()
	}
}

// runTransferEventsConsumer routes transfer.events into local WS pushes.
// kafka.LastOffset (set by the caller) is the deliberate opposite choice
// from the account.events reader above: a WS push is a live notification,
// not state to rebuild, and there is no connection to deliver a backfilled
// one to anyway — see newInstanceConsumerGroup's doc comment and README.
func runTransferEventsConsumer(fetchCtx context.Context, reader *kafka.Reader, cache *accountCache, registry *wsRegistry) {
	const topic = "transfer.events"
	defer reader.Close()
	for {
		msg, err := reader.FetchMessage(fetchCtx)
		if err != nil {
			if fetchCtx.Err() != nil {
				log.Printf("gateway: %s: shutting down", topic)
				return
			}
			log.Printf("gateway: %s: failed to fetch message: %v", topic, err)
			time.Sleep(fetchErrorBackoff)
			continue
		}
		// The span that closes the loop the DoD is about: a transfer's
		// delivery trace now runs relay -> Kafka -> gateway -> the
		// WebSocket frame that lands in the browser.
		ctx, span := otel.Tracer(tracerScope).Start(
			traceContextFromMessage(context.Background(), msg),
			"gateway handle "+topic,
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(
				attribute.String("messaging.system", "kafka"),
				attribute.String("messaging.source.name", topic),
				attribute.Int64("messaging.kafka.offset", msg.Offset),
			),
		)

		handleTransferMessage(ctx, msg, cache, registry)

		if err := reader.CommitMessages(ctx, msg); err != nil {
			log.Printf("gateway: %s: failed to commit offset at partition %d offset %d: %v", topic, msg.Partition, msg.Offset, err)
		}
		span.End()
	}
}

// handleTransferMessage unmarshals msg according to its event_type header
// and turns it into signals via notify.go's pure routing functions, then
// delivers each to whatever local WS connections that user has (possibly
// none — the common case on any given instance for any given event).
func handleTransferMessage(ctx context.Context, msg kafka.Message, cache *accountCache, registry *wsRegistry) {
	resolve := func(accountID string) (string, bool) { return cache.resolve(ctx, accountID) }

	var signals []wsSignal
	switch eventTypeOf(msg) {
	case eventTypeTransferCompleted:
		var event eventsv1.TransferCompleted
		if err := proto.Unmarshal(msg.Value, &event); err != nil {
			log.Printf("gateway: transfer.events: failed to unmarshal TransferCompleted at offset %d: %v", msg.Offset, err)
			return
		}
		signals = signalsForTransferEvent(eventTypeTransferCompleted, event.GetTransferId(), event.GetSenderAccountId(), event.GetRecipientAccountId(), resolve)
	case eventTypeTransferFailed:
		var event eventsv1.TransferFailed
		if err := proto.Unmarshal(msg.Value, &event); err != nil {
			log.Printf("gateway: transfer.events: failed to unmarshal TransferFailed at offset %d: %v", msg.Offset, err)
			return
		}
		signals = signalsForTransferEvent(eventTypeTransferFailed, event.GetTransferId(), event.GetSenderAccountId(), event.GetRecipientAccountId(), resolve)
	case eventTypeTransferRejected:
		var event eventsv1.TransferRejected
		if err := proto.Unmarshal(msg.Value, &event); err != nil {
			log.Printf("gateway: transfer.events: failed to unmarshal TransferRejected at offset %d: %v", msg.Offset, err)
			return
		}
		signals = signalsForTransferEvent(eventTypeTransferRejected, event.GetTransferId(), event.GetSenderAccountId(), event.GetRecipientAccountId(), resolve)
	case eventTypeDepositCredited:
		var event eventsv1.DepositCredited
		if err := proto.Unmarshal(msg.Value, &event); err != nil {
			log.Printf("gateway: transfer.events: failed to unmarshal DepositCredited at offset %d: %v", msg.Offset, err)
			return
		}
		signals = signalsForDepositEvent(event.GetDepositId(), event.GetAccountId(), resolve)
	case "":
		log.Printf("gateway: transfer.events: message at offset %d has no event_type header, skipping", msg.Offset)
		return
	default:
		// Forward-compat: a newer producer emitting a type this instance
		// doesn't know yet. Not an error — just nothing to route.
		return
	}

	for _, sig := range signals {
		registry.send(ctx, sig.userID, sig.msg)
	}
}

// startKafkaConsumers wires up both readers and launches their consumer
// goroutines, sharing one consumer group (Kafka tracks offsets per
// topic-partition within a group regardless of how many topics it spans,
// same reasoning as notifications-svc's newKafkaReader). Called once from
// main() — deliberately not from newHandler, which every existing test
// calls with context.Background() and would otherwise leak these
// goroutines and their Kafka dial attempts across the whole test binary.
func startKafkaConsumers(ctx context.Context, registry *wsRegistry, cache *accountCache) {
	brokers := strings.Split(envOr("KAFKA_BROKERS", defaultKafkaBrokers), ",")
	accountEventsTopic := envOr("KAFKA_ACCOUNT_EVENTS_TOPIC", defaultAccountEventsTopic)
	transferEventsTopic := envOr("KAFKA_TRANSFER_EVENTS_TOPIC", defaultTransferEventsTopic)
	groupID := newInstanceConsumerGroup()
	log.Printf("gateway: kafka consumer group %s", groupID)

	accountReader := newGatewayKafkaReader(brokers, groupID, accountEventsTopic, kafka.FirstOffset)
	go runAccountEventsConsumer(ctx, accountReader, cache)

	transferReader := newGatewayKafkaReader(brokers, groupID, transferEventsTopic, kafka.LastOffset)
	go runTransferEventsConsumer(ctx, transferReader, cache, registry)
}
