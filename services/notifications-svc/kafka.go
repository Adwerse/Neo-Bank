package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"neobank/pkg/pgha"
	"neobank/pkg/tracing"
	eventsv1 "neobank/proto/gen/go/events/v1"
)

// tracerScope names this service's instrumentation scope for the spans it
// creates by hand (the automatic HTTP/gRPC/pgx instrumentation supplies
// its own).
const tracerScope = "neobank/services/notifications-svc"

const notificationsConsumerGroup = "notifications-svc"

// contactWaitAttempts/Delay bound every in-process wait for the
// user_contacts projection to catch up — handleAccountCreated's wait for
// the corresponding UserActivated, and the transfer handlers' wait for
// the AccountCreated that links an account_id. See handleAccountCreated
// for why this can't just rely on Kafka redelivery.
const contactWaitAttempts = 15
const contactWaitDelay = 200 * time.Millisecond

// fetchErrorBackoff keeps a failing FetchMessage from becoming a hot
// spin. The bare `continue` this replaces would loop as fast as the
// broker could refuse — most plausibly against a topic that doesn't
// exist yet — burning CPU and drowning the log.
const fetchErrorBackoff = time.Second

// transferMaxAttempts bounds how many times runTransferEventsConsumer
// tries to fully process one transfer.events message — unmarshal,
// dispatch, and handle — before giving up on it and routing it to the
// dead letter topic. Without this, one message this service can never
// process (a payload that will never unmarshal, an address net/smtp
// permanently refuses) would previously just be logged once and silently
// lost the moment a later message's offset committed past it — not
// retried forever, and not blocking the partition either, since
// kafka-go's Reader always advances on FetchMessage regardless of commit
// state. See handleTransferMessageWithRetry.
const transferMaxAttempts = 5

// transferRetryBaseDelay is the sleep before the second attempt;
// transferRetryDelay doubles it on each subsequent attempt, capped at
// transferRetryMaxDelay so a long losing streak still reaches the DLQ in
// bounded time.
const transferRetryBaseDelay = 500 * time.Millisecond
const transferRetryMaxDelay = 8 * time.Second

// transferUnavailableBudget is how long handleTransferMessageWithRetry
// will keep retrying a message whose ONLY problem is that Postgres is
// currently unreachable, without spending any of its transferMaxAttempts
// poison budget.
//
// This exists because automatic failover broke an assumption the retry
// ladder above was built on. Five attempts at 0.5/1/2/4s cover about
// 7.5 seconds — which was a reasonable definition of "transient" when
// the only transient failure was a blip talking to Mailpit. A Patroni
// failover takes longer than that (detection alone is bounded by the
// cluster's ttl — see infra/patroni/patroni.yml), so without this the
// first failover would exhaust the ladder on every in-flight transfer
// notification and route a burst of perfectly good messages to the dead
// letter topic. Nothing would be lost — that is what the DLQ is for —
// but "every transfer during a failover needs a manual replay" is a bad
// enough outcome to design against, and it would look like a poison-
// message incident rather than the routine infrastructure event it is.
//
// Bounded rather than infinite so a Postgres that is down for good still
// ends up somewhere visible instead of wedging the partition silently
// forever. Five minutes is roughly twenty times the worst realistic
// failover and short enough that a genuine outage surfaces while someone
// is still looking at it.
const transferUnavailableBudget = 5 * time.Minute

// transferUnavailableRetryDelay is the flat poll interval while waiting
// out an unavailable database. Flat, not exponential: there is no
// thundering-herd concern (one goroutine, one message) and a fixed
// interval means the message resumes promptly whenever the new leader
// appears, rather than sitting out a backoff that grew while nobody was
// watching.
const transferUnavailableRetryDelay = 2 * time.Second

// consumerLagLogInterval is how often monitorConsumers reports each
// reader's lag.
const consumerLagLogInterval = 30 * time.Second

// kafkaHealthProbeInterval is how often monitorKafkaHealth dials the
// brokers to check reachability. Shorter than consumerLagLogInterval on
// purpose: the probe is a cheap, independent connection attempt, not
// something that needs to wait for a reader's own fetch cycle.
const kafkaHealthProbeInterval = 10 * time.Second

// kafkaHealthProbeTimeout bounds a single probe dial, so a broker that
// accepts a TCP connection but never completes the Kafka handshake can't
// wedge the health goroutine past the next tick.
const kafkaHealthProbeTimeout = 5 * time.Second

// eventTypeHeader is the Kafka message header carrying the event's type.
// It duplicates pkg/outbox's HeaderEventType literal rather than
// importing that module: what's needed here is one string, and
// pkg/outbox is the PUBLISHING side (write into an outbox table in the
// same transaction, then relay it). notifications-svc owns no outbox
// table for the topics it reads and publishes nothing there; a pure
// consumer depending on the producers' library would invert the layering
// and mislead the next reader into thinking this service emits those
// events. (This service does now publish to one topic of its own, the
// DLQ — see kafkaMessageWriter — but that doesn't change the reasoning
// for the events it reads.) pkg/outbox's TestHeaderEventType_IsWireContract
// pins the literal there so a rename fails loudly on the producing side
// instead of silently producing a topic nobody can route.
const eventTypeHeader = "event_type"

// The event_type values transfers-svc writes into its outbox rows, which
// the relay copies verbatim onto the header. DepositCredited shares this
// same outbox table and transfer.events topic as the three transfer
// events — deposits and transfers are both money-movement events with
// external uncertainty that transfers-svc already has the outbox/relay
// infrastructure for, and multiplexing one more type over the existing
// header-discriminated topic avoids standing up a second physical outbox
// table, Kafka topic, relay, and consumer goroutine for a single event
// type (see transfers-svc's README for the fuller reasoning).
const (
	eventTypeTransferCompleted = "TransferCompleted"
	eventTypeTransferFailed    = "TransferFailed"
	eventTypeTransferRejected  = "TransferRejected"
	eventTypeDepositCredited   = "DepositCredited"
)

// user.events carries two event types from auth-svc's single auth_outbox
// table: UserActivated (since sprint 2) and, as of the profile sprint,
// ProfileUpdated — see README's mini-ADR for why profile data reuses this
// same outbox/topic rather than getting its own. Unlike
// TransferCompleted/Failed/Rejected, these two don't share field numbers
// (there's no accidental-cross-decode risk), but the header is still the
// only way to know which one a given message is without guessing.
const (
	eventTypeUserActivated  = "UserActivated"
	eventTypeProfileUpdated = "ProfileUpdated"
)

// newProjectionReader and newNotificationReader differ in exactly one
// setting, StartOffset, and that difference is the most consequential
// choice in this service.
//
// A projection reader (user.events, account.events) starts at
// FirstOffset. user_contacts is state: replaying the whole compacted log
// rebuilds it, and re-applying an upsert costs nothing. This group also
// subscribed to user.events long after UserActivated started flowing (in
// sprint 2), so without an explicit earliest start every user activated
// before this service existed would never make it into the projection.
// See README's "Kafka: offset reset и retention" for why compaction on
// those two topics is the necessary other half of that.
//
// A notification reader (transfer.events) starts at LastOffset, and must.
// That topic is NOT compacted, carries ordinary time-based retention, and
// has been accumulating events since sprint 5. FirstOffset on a brand-new
// group would replay all of them — and the idempotency barrier would stop
// exactly none of it, since notifications_processed_events holds no rows
// for those event_ids — mailing every user about every transfer they made
// weeks ago, twice over for each completed one. An email is a side effect
// in the outside world, not a state update: there is no "rebuilding" one,
// and history is precisely what must NOT be replayed.
//
// StartOffset applies only while the group has no committed offset on a
// partition, so this costs nothing after the first start — a crash,
// restart, or deliberate offset reset all resume from the committed
// position. The price is one real gap: a transfer completing during the
// very first startup, before the group commits anything, gets no email.
// Once, on one deploy, in exchange for not spamming every user on that
// same deploy.
func newProjectionReader(brokers, topic string) *kafka.Reader {
	return newKafkaReader(brokers, topic, kafka.FirstOffset)
}

func newNotificationReader(brokers, topic string) *kafka.Reader {
	return newKafkaReader(brokers, topic, kafka.LastOffset)
}

// newKafkaReader constructs one of notifications-svc's consumers. There
// is one reader per topic, each with its own goroutine, since kafka-go's
// Reader subscribes to exactly one topic; all share the same GroupID,
// which is fine — Kafka tracks committed offsets per topic-partition
// within a group regardless of how many topics that group spans.
func newKafkaReader(brokers, topic string, startOffset int64) *kafka.Reader {
	return kafka.NewReader(kafka.ReaderConfig{
		Brokers:     strings.Split(brokers, ","),
		Topic:       topic,
		GroupID:     notificationsConsumerGroup,
		StartOffset: startOffset,
	})
}

// newKafkaWriter constructs this service's one producer — the dead
// letter topic. Same shape as transfers-svc's newKafkaWriter:
// AllowAutoTopicCreation must be set true client-side even though the
// broker already has auto-creation enabled, and Balancer is set
// explicitly (rather than the zero-value default, which ignores the
// message key) so a DLQ'd event's key — the same partition key its
// source topic used — still groups by account the way an operator
// skimming the DLQ would expect.
func newKafkaWriter(brokers, topic string) *kafka.Writer {
	return &kafka.Writer{
		Addr:                   kafka.TCP(strings.Split(brokers, ",")...),
		Topic:                  topic,
		Balancer:               &kafka.Hash{},
		AllowAutoTopicCreation: true,
	}
}

// kafkaMessageWriter is the narrow slice of *kafka.Writer's API sendToDLQ
// needs, narrowed to an interface so tests can substitute a fake instead
// of a live broker. *kafka.Writer satisfies this implicitly. Same shape
// as pkg/outbox.KafkaMessageWriter, duplicated rather than imported — see
// eventTypeHeader's doc comment for why this service avoids depending on
// pkg/outbox.
type kafkaMessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafka.Message) error
}

// consumerHealth tracks whether this service can currently reach the
// Kafka brokers, so /healthz can report Kafka connectivity honestly
// instead of the process's mere liveness. Zero value is false —
// "reachable" — so a freshly constructed consumerHealth reads as healthy
// before monitorKafkaHealth's first probe.
//
// One flag for the whole broker set, not one per reader: this repo's
// dev stack is a single broker, and all three readers share the same
// Brokers list, so "is Kafka reachable" is one fact, not three. See
// monitorKafkaHealth for how it's determined and why not from the
// readers themselves.
type consumerHealth struct {
	unreachable atomic.Bool
}

func (h *consumerHealth) ok() bool {
	return !h.unreachable.Load()
}

// eventTypeOf reads the discriminator the outbox relay stamps on every
// message. Returns "" when the header is absent — which means the message
// predates the relay change, since nothing publishes without it now.
//
// Case-sensitive on purpose: Kafka header keys are opaque bytes and the
// producer writes exactly "event_type", so a near-miss is a bug to
// surface, not to paper over.
func eventTypeOf(msg kafka.Message) string {
	for _, h := range msg.Headers {
		if h.Key == eventTypeHeader {
			return string(h.Value)
		}
	}
	return ""
}

// commitMessage centralizes the "commit, log if that fails, carry on"
// tail every consumer loop ends with. A failed commit is not fatal: the
// worst case is redelivery, which every handler here is built to absorb.
func commitMessage(ctx context.Context, reader *kafka.Reader, msg kafka.Message, topic string) {
	if err := reader.CommitMessages(ctx, msg); err != nil {
		log.Printf("notifications-svc: %s: failed to commit offset at partition %d offset %d: %v", topic, msg.Partition, msg.Offset, err)
	}
}

// runUserEventsConsumer and runAccountCreatedConsumer follow the same
// at-least-once shape as accounts-svc's runUserActivatedConsumer:
// FetchMessage + explicit CommitMessages, only after the handler
// succeeds, so a crash or DB error between fetch and commit leaves the
// message uncommitted for redelivery rather than silently skipped.
//
// fetchCtx is the shutdown signal, not a deadline on the work a fetched
// message triggers: FetchMessage blocks on it, so cancelling it (SIGTERM,
// via main) unblocks a reader that is idle waiting for the next message
// and this loop returns without starting new work. A message already
// fetched is still handled and committed using context.Background(), not
// fetchCtx, so it runs to completion even if shutdown was signalled a
// moment after the fetch returned — see runTransferEventsConsumer's doc
// comment, which spells this out in more depth since that loop also owns
// a retry/DLQ decision.
//
// Dispatches by event_type header — user.events has carried more than
// one message type since ProfileUpdated started sharing auth-svc's
// outbox with UserActivated. No header (eventTypeOf returns "") is
// treated as UserActivated, not rejected: every message on this topic
// before ProfileUpdated existed WAS one, so that's the correct read of a
// legacy pre-header message here — unlike transfer.events, where
// UserActivated's sibling event types genuinely can't be told apart
// without the header and guessing would be wrong.
func runUserEventsConsumer(fetchCtx context.Context, reader *kafka.Reader, pool *pgxpool.Pool) {
	const topic = "user.events"
	for {
		msg, err := reader.FetchMessage(fetchCtx)
		if err != nil {
			if fetchCtx.Err() != nil {
				log.Printf("notifications-svc: %s: shutting down", topic)
				return
			}
			log.Printf("notifications-svc: %s: failed to fetch message: %v", topic, err)
			time.Sleep(fetchErrorBackoff)
			continue
		}
		// Join the delivery trace the relay started, so this projection
		// write appears under the same trace as the publish that caused
		// it rather than as an orphan.
		ctx, span := otel.Tracer(tracerScope).Start(
			traceContextFromMessage(context.Background(), msg),
			"notifications handle "+topic,
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(
				attribute.String("messaging.system", "kafka"),
				attribute.String("messaging.source.name", topic),
				attribute.Int64("messaging.kafka.offset", msg.Offset),
			),
		)

		switch eventType := eventTypeOf(msg); eventType {
		case "", eventTypeUserActivated:
			var event eventsv1.UserActivated
			if err := proto.Unmarshal(msg.Value, &event); err != nil {
				log.Printf("notifications-svc: %s: failed to unmarshal UserActivated at offset %d: %v", topic, msg.Offset, err)
				tracing.Fail(ctx, "unmarshal_failed", err)
				// No amount of redelivery makes an unparseable payload
				// parseable — commit it so a poison message doesn't block
				// the partition forever.
				commitMessage(ctx, reader, msg, topic)
				span.End()
				continue
			}
			if err := handleUserActivated(ctx, pool, &event); err != nil {
				log.Printf("notifications-svc: failed to handle UserActivated event %s for user %s: %v", event.GetEventId(), event.GetUserId(), err)
				tracing.Fail(ctx, "useractivated_handling_failed", err)
				span.End()
				continue // do not commit — message will be redelivered
			}

		case eventTypeProfileUpdated:
			var event eventsv1.ProfileUpdated
			if err := proto.Unmarshal(msg.Value, &event); err != nil {
				log.Printf("notifications-svc: %s: failed to unmarshal ProfileUpdated at offset %d: %v", topic, msg.Offset, err)
				tracing.Fail(ctx, "unmarshal_failed", err)
				commitMessage(ctx, reader, msg, topic)
				span.End()
				continue
			}
			if err := handleProfileUpdated(ctx, pool, &event); err != nil {
				log.Printf("notifications-svc: failed to handle ProfileUpdated event %s for user %s: %v", event.GetEventId(), event.GetUserId(), err)
				tracing.Fail(ctx, "profileupdated_handling_failed", err)
				span.End()
				continue // do not commit — message will be redelivered
			}

		default:
			// Forward compatibility, same policy as transfer.events'
			// unknown-type branch: a type a later sprint adds must not
			// permanently wedge an older binary's partition. This topic
			// has no DLQ, so — matching this topic's existing (pre-header)
			// poison handling — commit past it rather than spin forever.
			log.Printf("notifications-svc: %s: unknown event_type %q at offset %d, skipping", topic, eventType, msg.Offset)
			tracing.Fail(ctx, "unknown_event_type", fmt.Errorf("unknown event_type %q", eventType))
			commitMessage(ctx, reader, msg, topic)
			span.End()
			continue
		}

		commitMessage(ctx, reader, msg, topic)
		span.End()
	}
}

func runAccountCreatedConsumer(fetchCtx context.Context, reader *kafka.Reader, pool *pgxpool.Pool) {
	const topic = "account.events"
	for {
		msg, err := reader.FetchMessage(fetchCtx)
		if err != nil {
			if fetchCtx.Err() != nil {
				log.Printf("notifications-svc: %s: shutting down", topic)
				return
			}
			log.Printf("notifications-svc: %s: failed to fetch message: %v", topic, err)
			time.Sleep(fetchErrorBackoff)
			continue
		}
		// Join the delivery trace the relay started, so this projection
		// write appears under the same trace as the publish that caused
		// it rather than as an orphan.
		ctx, span := otel.Tracer(tracerScope).Start(
			traceContextFromMessage(context.Background(), msg),
			"notifications handle "+topic,
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(
				attribute.String("messaging.system", "kafka"),
				attribute.String("messaging.source.name", topic),
				attribute.Int64("messaging.kafka.offset", msg.Offset),
			),
		)

		var event eventsv1.AccountCreated
		if err := proto.Unmarshal(msg.Value, &event); err != nil {
			log.Printf("notifications-svc: %s: failed to unmarshal AccountCreated at offset %d: %v", topic, msg.Offset, err)
			tracing.Fail(ctx, "unmarshal_failed", err)
			commitMessage(ctx, reader, msg, topic)
			span.End()
			continue
		}

		if err := handleAccountCreated(ctx, pool, &event); err != nil {
			log.Printf("notifications-svc: failed to handle AccountCreated event %s for user %s: %v", event.GetEventId(), event.GetUserId(), err)
			tracing.Fail(ctx, "accountcreated_handling_failed", err)
			span.End()
			continue
		}

		commitMessage(ctx, reader, msg, topic)
		span.End()
	}
}

// handleUserActivated is idempotent: safe to call more than once for the
// same event_id, same reasoning as accounts-svc's handleUserActivated.
// Ordering is deliberate: check processed_events first (fast path), do
// the actual upsert, and only mark processed_events LAST, strictly after
// the projection write has actually landed — a crash between the upsert
// and this bookkeeping write must leave processed_events empty, so
// redelivery genuinely retries rather than being wrongly skipped.
func handleUserActivated(ctx context.Context, pool *pgxpool.Pool, event *eventsv1.UserActivated) error {
	eventID := event.GetEventId()

	processed, err := isEventProcessed(ctx, pool, eventID)
	if err != nil {
		return fmt.Errorf("check processed_events for event %s: %w", eventID, err)
	}
	if processed {
		log.Printf("notifications-svc: event %s already processed, skipping (redelivery)", eventID)
		return nil
	}

	if err := upsertUserContactEmail(ctx, pool, event.GetUserId(), event.GetEmail()); err != nil {
		return fmt.Errorf("upsert user_contacts for user %s: %w", event.GetUserId(), err)
	}

	// 'skipped': UserActivated never produces a notification, it only
	// feeds the projection. auth-svc already emails this user directly
	// during registration; the events this service turns into mail are
	// the transfer ones.
	if err := markEventProcessed(ctx, pool, eventID, eventStatusSkipped); err != nil {
		return fmt.Errorf("mark event %s processed: %w", eventID, err)
	}
	return nil
}

// handleProfileUpdated is idempotent for the same reason handleUserActivated
// is, and follows the identical shape: fast-path check, apply the
// projection write, mark processed_events LAST. It never produces a
// notification either — it only feeds the projection so a later
// transfer/deposit email can address the recipient by name; sending mail
// ABOUT a profile change was never asked for and isn't done here.
func handleProfileUpdated(ctx context.Context, pool *pgxpool.Pool, event *eventsv1.ProfileUpdated) error {
	eventID := event.GetEventId()

	processed, err := isEventProcessed(ctx, pool, eventID)
	if err != nil {
		return fmt.Errorf("check processed_events for event %s: %w", eventID, err)
	}
	if processed {
		log.Printf("notifications-svc: event %s already processed, skipping (redelivery)", eventID)
		return nil
	}

	if err := updateUserContactDisplayName(ctx, pool, event.GetUserId(), event.GetDisplayName()); err != nil {
		return fmt.Errorf("update user_contacts display_name for user %s: %w", event.GetUserId(), err)
	}

	if err := markEventProcessed(ctx, pool, eventID, eventStatusSkipped); err != nil {
		return fmt.Errorf("mark event %s processed: %w", eventID, err)
	}
	return nil
}

// handleAccountCreated links accountID into an existing user_contacts row.
// Unlike handleUserActivated, "the row doesn't exist yet" is a real,
// expected possibility here — AccountCreated is causally downstream of
// UserActivated (accounts-svc only ever creates an account in response to
// consuming UserActivated), but the two travel on independent topics with
// independent consumer goroutines in this process, so there's no
// ordering guarantee between when each gets processed.
//
// This retries in place, with a short sleep, rather than returning an
// error and hoping Kafka redelivers: kafka-go's Reader has no per-message
// redelivery within a running process — FetchMessage always advances to
// the next message regardless of whether the previous one was committed,
// so "return an error, don't commit" only helps if the whole process
// restarts before anything later on this topic gets committed. Given the
// causal relationship, the wait here is normally milliseconds; the bound
// exists so a genuinely stuck case (e.g. data corruption) doesn't block
// this consumer's goroutine forever — it logs and gives up for this pass,
// falling back to Kafka-level redelivery on a future restart as a last
// resort, imperfect as that is once later messages have committed past it.
func handleAccountCreated(ctx context.Context, pool *pgxpool.Pool, event *eventsv1.AccountCreated) error {
	eventID := event.GetEventId()

	processed, err := isEventProcessed(ctx, pool, eventID)
	if err != nil {
		return fmt.Errorf("check processed_events for event %s: %w", eventID, err)
	}
	if processed {
		log.Printf("notifications-svc: event %s already processed, skipping (redelivery)", eventID)
		return nil
	}

	var linked bool
	for attempt := 0; attempt < contactWaitAttempts; attempt++ {
		linked, err = updateUserContactAccountLink(ctx, pool, event.GetUserId(), event.GetAccountId(), event.GetAccountNumber())
		if err != nil {
			return fmt.Errorf("link account for user %s: %w", event.GetUserId(), err)
		}
		if linked {
			break
		}
		time.Sleep(contactWaitDelay)
	}
	if !linked {
		return fmt.Errorf("no user_contacts row for user %s after %d attempts (event %s, account %s): giving up for this pass", event.GetUserId(), contactWaitAttempts, eventID, event.GetAccountId())
	}

	if err := markEventProcessed(ctx, pool, eventID, eventStatusSkipped); err != nil {
		return fmt.Errorf("mark event %s processed: %w", eventID, err)
	}
	return nil
}

// runTransferEventsConsumer is the one loop in this service whose
// handlers reach outside the process, and the only one backed by a dead
// letter topic.
//
// fetchCtx is the shutdown signal, exactly as in runUserEventsConsumer:
// FetchMessage blocks on it, so cancelling it unblocks an idle reader and
// this loop returns without starting new work. Once a message IS in
// hand, though, it is handed to handleTransferMessageWithRetry with a
// fresh context.Background(), not fetchCtx — a message already being
// processed runs to completion (every retry attempt, a possible DLQ
// publish, and the final commit) even if shutdown was signalled a moment
// after the fetch returned. That is what "finish the current message
// before exiting" means operationally: the boundary is per-message, not
// per-goroutine, and it would be wrong to let a shutdown signal abort a
// handler mid-send — that's how a message ends up neither committed nor
// fully handled.
func runTransferEventsConsumer(fetchCtx context.Context, reader *kafka.Reader, pool *pgxpool.Pool, smtpAddr, smtpFrom string, dlqWriter kafkaMessageWriter, dlqTopic string) {
	const topic = "transfer.events"
	for {
		msg, err := reader.FetchMessage(fetchCtx)
		if err != nil {
			if fetchCtx.Err() != nil {
				log.Printf("notifications-svc: %s: shutting down", topic)
				return
			}
			log.Printf("notifications-svc: %s: failed to fetch message: %v", topic, err)
			time.Sleep(fetchErrorBackoff)
			continue
		}

		// context.Background() and not fetchCtx, for the reason this
		// function's doc comment gives — then the delivery trace is
		// grafted onto it from the message headers, so this consumer's
		// spans join the trace the relay started when it published,
		// rather than rooting a new one per message.
		handleTransferMessageWithRetry(traceContextFromMessage(context.Background(), msg), reader, msg, pool, smtpAddr, smtpFrom, dlqWriter, dlqTopic)
	}
}

// traceContextFromMessage grafts the trace context the outbox relay
// injected into the message's headers onto ctx.
//
// Duplicated in gateway and accounts-svc rather than shared, matching how
// this repo already duplicates newKafkaWriter and the event_type header
// constant across service modules: the shared thing is the wire format,
// not the code.
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

// processTransferMessage unmarshals msg according to its event_type
// header and runs the matching handler. It is the unit
// handleTransferMessageWithRetry retries as a whole — unmarshal and
// handle together — so a retry after a transient failure re-derives the
// event from the original bytes rather than trusting state left over from
// a failed previous attempt.
//
// eventID is returned whenever it could be determined — i.e. unmarshal
// succeeded — even when err is also non-nil, so a caller that gives up
// after exhausting retries can still close out a notifications_processed_events
// barrier row a partially-completed handler left at 'processing'. It is
// "" when the message never got that far: no header, an unrecognized
// header, or a payload proto.Unmarshal itself rejected — cases with no
// event_id to key that table with at all.
//
// transfer.events carries three message types, and protobuf's wire
// format cannot tell them apart: TransferCompleted/Failed/Rejected share
// field numbers and types for fields 1-5, and field 6 is a string in all
// three, so decoding any one of them as any other SUCCEEDS and quietly
// files a failure reason under LedgerTransactionId. The event_type header
// the outbox relay stamps is the only discriminator; see eventTypeHeader.
func processTransferMessage(ctx context.Context, pool *pgxpool.Pool, smtpAddr, smtpFrom string, msg kafka.Message) (eventID, eventType string, err error) {
	eventType = eventTypeOf(msg)

	switch eventType {
	case eventTypeTransferCompleted:
		var event eventsv1.TransferCompleted
		if err := proto.Unmarshal(msg.Value, &event); err != nil {
			return "", eventType, fmt.Errorf("unmarshal %s at offset %d: %w", eventType, msg.Offset, err)
		}
		return event.GetEventId(), eventType, handleTransferCompleted(ctx, pool, smtpAddr, smtpFrom, &event)

	case eventTypeTransferFailed:
		var event eventsv1.TransferFailed
		if err := proto.Unmarshal(msg.Value, &event); err != nil {
			return "", eventType, fmt.Errorf("unmarshal %s at offset %d: %w", eventType, msg.Offset, err)
		}
		return event.GetEventId(), eventType, handleTransferFailed(ctx, pool, smtpAddr, smtpFrom, &event)

	case eventTypeTransferRejected:
		var event eventsv1.TransferRejected
		if err := proto.Unmarshal(msg.Value, &event); err != nil {
			return "", eventType, fmt.Errorf("unmarshal %s at offset %d: %w", eventType, msg.Offset, err)
		}
		return event.GetEventId(), eventType, handleTransferRejected(ctx, pool, smtpAddr, smtpFrom, &event)

	case eventTypeDepositCredited:
		var event eventsv1.DepositCredited
		if err := proto.Unmarshal(msg.Value, &event); err != nil {
			return "", eventType, fmt.Errorf("unmarshal %s at offset %d: %w", eventType, msg.Offset, err)
		}
		return event.GetEventId(), eventType, handleDepositCredited(ctx, pool, smtpAddr, smtpFrom, &event)

	case "":
		// Published before the relay started stamping the header. An old
		// message will never grow one, so this is structurally poison —
		// no number of retries changes the outcome, but it still goes
		// through the same retry-then-DLQ path as everything else rather
		// than a special-cased immediate commit, so the DLQ (not just a
		// log line) ends up holding the bytes.
		//
		// Deliberately NOT unmarshaled as TransferCompleted just to
		// recover an event_id for this error, tempting as that is with
		// fields 1-5 identical: that is exactly the accidental
		// cross-decode the header exists to prevent. Partition and
		// offset are what an operator needs to inspect it anyway, and
		// both travel with the DLQ'd message.
		return "", eventType, fmt.Errorf("message at partition %d offset %d has no %q header (published before the discriminator existed)", msg.Partition, msg.Offset, eventTypeHeader)

	default:
		// Forward compatibility: a type a later sprint adds must not
		// permanently wedge an older binary's partition. It retries like
		// anything else (harmlessly futile against an older binary) and
		// lands in the DLQ, where a redeployed binary that knows the type
		// — or a human — can replay it.
		return "", eventType, fmt.Errorf("unknown event_type %q at partition %d offset %d", eventType, msg.Partition, msg.Offset)
	}
}

// transferRetryDelay is the exponential backoff between attempts:
// transferRetryBaseDelay doubled per attempt, capped at
// transferRetryMaxDelay.
func transferRetryDelay(attempt int) time.Duration {
	d := transferRetryBaseDelay * time.Duration(uint64(1)<<uint(attempt-1))
	if d > transferRetryMaxDelay {
		return transferRetryMaxDelay
	}
	return d
}

// handleTransferMessageWithRetry is the durability boundary for
// transfer.events: it owns the decision between "try again" and "give up
// and move on," which the old runTransferEventsConsumer used to make only
// implicitly, and not quite correctly — "don't commit on error" does not
// actually retry anything here, since kafka-go's Reader always advances
// past a fetched message regardless of commit state. A failure on message
// N followed by success on message N+1 committed right past N, losing it
// silently the moment that later commit landed.
//
// This retries the SAME message up to transferMaxAttempts times with
// exponential backoff, calling processTransferMessage fresh each time.
// That re-entrancy is safe because of claimEvent: an attempt that got far
// enough to claim the event leaves the barrier row at 'processing' on
// failure, and the next attempt's claimEvent call reclaims that same row
// rather than treating it as already handled — the "duplicate over loss"
// policy this service already applies to a crash mid-send now also
// covers a retried transient failure. See contacts.go's claimEvent.
//
// Exhausting every attempt means this message is poison — a payload that
// will never unmarshal, an address SMTP permanently refuses, or a
// downstream dependency that stayed down for the entire retry window.
// Rather than lose it (the old behavior for an unmarshal failure) or park
// this goroutine on it forever (blocking every transfer notification
// behind it), it is republished verbatim to dlqTopic with the failure
// reason attached as a header, any barrier row is closed out as
// 'skipped', and the offset is committed so the partition moves on.
// Nothing is lost — the DLQ holds the original bytes for a human to
// inspect and, if the fix is on this service's side, replay by hand.
//
// A permanently-missing contact does NOT reach this function as an error:
// waitForContactByAccountID treats "still not found after its own bounded
// wait" as a terminal, non-error outcome (see its doc comment), so it
// never enters this retry loop and never lands in the DLQ. That is
// deliberate — a contact that never appears means the projection itself
// is inconsistent, not that this message is malformed, and a DLQ built
// for "an operator can fix and replay this" is the wrong place for it.
//
// Neither does a database that is merely unreachable. Since the cluster
// gained automatic failover, "Postgres refused this write" no longer
// implies anything about the message: it usually means Patroni is
// electing a leader. Those attempts are retried on their own budget
// (transferUnavailableBudget) and do not count toward transferMaxAttempts
// — see pgha.IsUnavailable for exactly which errors qualify, and why
// separating the two matters more here than anywhere else in the repo.
func handleTransferMessageWithRetry(ctx context.Context, reader *kafka.Reader, msg kafka.Message, pool *pgxpool.Pool, smtpAddr, smtpFrom string, dlqWriter kafkaMessageWriter, dlqTopic string) {
	const topic = "transfer.events"
	var eventID, eventType string
	var err error

	// One span for the whole handling of this message — every attempt,
	// the DLQ decision, and the commit — as a child of the delivery trace
	// the relay started. Its duration is the honest "how long did this
	// notification take to deliver", retries included.
	ctx, span := otel.Tracer(tracerScope).Start(ctx, "notifications handle "+topic,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "kafka"),
			attribute.String("messaging.source.name", topic),
			attribute.Int("messaging.kafka.partition", msg.Partition),
			attribute.Int64("messaging.kafka.offset", msg.Offset),
		),
	)
	defer span.End()

	unavailableDeadline := time.Now().Add(transferUnavailableBudget)
	attempts := 0

	for attempt := 1; attempt <= transferMaxAttempts; {
		attempts++
		// A span per attempt, so a message that was retried three times
		// and then dead-lettered reads as exactly that in Jaeger: three
		// sibling spans with rising attempt numbers, then a DLQ span.
		attemptCtx, attemptSpan := otel.Tracer(tracerScope).Start(ctx, "attempt",
			trace.WithAttributes(attribute.Int("neobank.retry.attempt", attempt)))

		eventID, eventType, err = processTransferMessage(attemptCtx, pool, smtpAddr, smtpFrom, msg)
		if err != nil {
			tracing.Fail(attemptCtx, "transfer_notification_attempt_failed", err)
		}
		attemptSpan.End()

		if err == nil {
			span.SetAttributes(
				attribute.Int("neobank.retry.attempts_used", attempts),
				attribute.String("neobank.event.type", eventType),
			)
			commitMessage(ctx, reader, msg, topic)
			return
		}

		// The database being unavailable says nothing about this
		// message, so it must not spend an attempt — otherwise a
		// failover looks identical to poison. `attempt` is deliberately
		// not incremented on this path, which is also why the loop's
		// post statement is empty and the increment lives below.
		if pgha.IsUnavailable(err) {
			if remaining := time.Until(unavailableDeadline); remaining > 0 {
				log.Printf("notifications-svc: %s: postgres unavailable while handling %s at offset %d, retrying in %s (%s of budget left, attempt %d/%d unspent): %v",
					topic, eventType, msg.Offset, transferUnavailableRetryDelay, remaining.Truncate(time.Second), attempt, transferMaxAttempts, err)
				time.Sleep(transferUnavailableRetryDelay)
				continue
			}
			// Budget exhausted: this is no longer "a failover is in
			// progress", it is an outage. Fall through to the DLQ so the
			// partition can move on, with a log line that says which of
			// the two it was.
			log.Printf("notifications-svc: %s: postgres still unavailable after %s for %s at offset %d — treating as an outage, not a transient failover",
				topic, transferUnavailableBudget, eventType, msg.Offset)
			break
		}

		log.Printf("notifications-svc: %s: attempt %d/%d failed for %s at offset %d: %v", topic, attempt, transferMaxAttempts, eventType, msg.Offset, err)
		if attempt < transferMaxAttempts {
			time.Sleep(transferRetryDelay(attempt))
		}
		attempt++
	}

	log.Printf("notifications-svc: %s: giving up on %s at offset %d, sending to %s: %v", topic, eventType, msg.Offset, dlqTopic, err)

	// Reaching the dead letter topic IS a failure of delivery, so unlike
	// a fraud rejection this does mark the span as an error — "show me
	// every notification that ended up in the DLQ" is exactly the query
	// this needs to answer.
	span.SetAttributes(
		attribute.Int("neobank.retry.attempts_used", attempts),
		attribute.Bool("neobank.dlq.sent", true),
		attribute.String("neobank.event.type", eventType),
	)
	tracing.Fail(ctx, "transfer_notification_dead_lettered", err)

	dlqCtx, dlqSpan := otel.Tracer(tracerScope).Start(ctx, "dlq publish",
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "kafka"),
			attribute.String("messaging.destination.name", dlqTopic),
		),
	)
	sendToDLQ(dlqCtx, dlqWriter, dlqTopic, msg, eventType, err)
	dlqSpan.End()

	if eventID != "" {
		if err := finishEvent(ctx, pool, eventID, eventStatusSkipped); err != nil {
			log.Printf("notifications-svc: %s: failed to close out barrier row for event %s after DLQ: %v", topic, eventID, err)
		}
	}
	commitMessage(ctx, reader, msg, topic)
}

// sendToDLQ republishes msg to the dead letter topic unchanged — same
// key, same value, same original headers — plus three headers recording
// why and where it came from, so an operator (or a replay tool) can
// inspect it without needing log access.
func sendToDLQ(ctx context.Context, writer kafkaMessageWriter, dlqTopic string, msg kafka.Message, eventType string, cause error) {
	headers := make([]kafka.Header, len(msg.Headers), len(msg.Headers)+3)
	copy(headers, msg.Headers)
	headers = append(headers,
		kafka.Header{Key: "dlq_reason", Value: []byte(cause.Error())},
		kafka.Header{Key: "dlq_source_partition", Value: []byte(strconv.Itoa(msg.Partition))},
		kafka.Header{Key: "dlq_source_offset", Value: []byte(strconv.FormatInt(msg.Offset, 10))},
	)

	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := writer.WriteMessages(writeCtx, kafka.Message{
		Key:     msg.Key,
		Value:   msg.Value,
		Headers: headers,
	}); err != nil {
		// The source offset still commits regardless (see the caller) —
		// otherwise a DLQ outage would turn into an infinite retry loop
		// on a message we already know is poison. This log line is, in
		// that case, the only remaining record of it.
		log.Printf("notifications-svc: %s: failed to publish poison message (offset %d, type %s) to %s: %v", "transfer.events", msg.Offset, eventType, dlqTopic, err)
	}
}

// handleTransferCompleted is the only event that produces TWO emails: the
// sender is told the money left, the recipient that it arrived. One
// event, two side effects — and exactly ONE barrier row, which is the
// interesting part.
//
// That single row is a deliberate trade. It makes "was this event
// handled?" one unambiguous fact, but it cannot distinguish "sent both"
// from "sent one". A crash between the two sends leaves the row at
// 'processing', and the replay re-sends BOTH, duplicating the first.
// That's the same direction claimEvent chose everywhere else — duplicate
// over loss — and the alternative (a barrier row per recipient) would
// trade it for a schema where "one row per event" no longer holds.
//
// The sender is mailed first: they initiated this and are waiting on the
// confirmation, so if only one message escapes before something breaks,
// it should be theirs.
func handleTransferCompleted(ctx context.Context, pool *pgxpool.Pool, smtpAddr, smtpFrom string, event *eventsv1.TransferCompleted) error {
	eventID := event.GetEventId()
	claimed, err := claimEvent(ctx, pool, eventID)
	if err != nil {
		return fmt.Errorf("claim event %s: %w", eventID, err)
	}
	if !claimed {
		log.Printf("notifications-svc: event %s already handled, skipping (redelivery)", eventID)
		return nil
	}

	sender, senderFound, err := waitForContactByAccountID(ctx, pool, event.GetSenderAccountId())
	if err != nil {
		return fmt.Errorf("look up sender contact for event %s: %w", eventID, err)
	}
	recipient, recipientFound, err := waitForContactByAccountID(ctx, pool, event.GetRecipientAccountId())
	if err != nil {
		return fmt.Errorf("look up recipient contact for event %s: %w", eventID, err)
	}

	occurred := eventTime(event.GetOccurredAt())
	sent := 0

	if senderFound {
		m := buildTransferSentEmail(event.GetAmount(), event.GetTransferId(), recipient.AccountNumber, occurred)
		if err := sendEmailWithRetry(smtpAddr, smtpFrom, sender.Email, m); err != nil {
			return fmt.Errorf("send transfer-sent email for event %s: %w", eventID, err)
		}
		sent++
	} else {
		log.Printf("notifications-svc: event %s: no contact for sender account %s, no transfer-sent email", eventID, event.GetSenderAccountId())
	}

	// sender.AccountNumber is "" when the sender wasn't found, and "" is
	// exactly what makes buildTransferReceivedEmail drop the From account
	// line — the recipient still learns money arrived. That builder is
	// given neither the sender's email address nor any balance.
	if recipientFound {
		m := buildTransferReceivedEmail(event.GetAmount(), event.GetTransferId(), sender.AccountNumber, occurred)
		if err := sendEmailWithRetry(smtpAddr, smtpFrom, recipient.Email, m); err != nil {
			return fmt.Errorf("send transfer-received email for event %s: %w", eventID, err)
		}
		sent++
	} else {
		log.Printf("notifications-svc: event %s: no contact for recipient account %s, no transfer-received email", eventID, event.GetRecipientAccountId())
	}

	return finishTransferEvent(ctx, pool, eventID, sent)
}

// handleTransferFailed mails the sender only. The ledger declined and no
// money moved, so the recipient has nothing to be told — which is also
// why this never looks the recipient up: that would be a wasted query and
// a wasted multi-second projection wait for an address it would discard.
func handleTransferFailed(ctx context.Context, pool *pgxpool.Pool, smtpAddr, smtpFrom string, event *eventsv1.TransferFailed) error {
	eventID := event.GetEventId()
	claimed, err := claimEvent(ctx, pool, eventID)
	if err != nil {
		return fmt.Errorf("claim event %s: %w", eventID, err)
	}
	if !claimed {
		log.Printf("notifications-svc: event %s already handled, skipping (redelivery)", eventID)
		return nil
	}

	sender, found, err := waitForContactByAccountID(ctx, pool, event.GetSenderAccountId())
	if err != nil {
		return fmt.Errorf("look up sender contact for event %s: %w", eventID, err)
	}
	sent := 0
	if found {
		m := buildTransferFailedEmail(event.GetAmount(), event.GetTransferId(), event.GetReason(), eventTime(event.GetOccurredAt()))
		if err := sendEmailWithRetry(smtpAddr, smtpFrom, sender.Email, m); err != nil {
			return fmt.Errorf("send transfer-failed email for event %s: %w", eventID, err)
		}
		sent++
	} else {
		log.Printf("notifications-svc: event %s: no contact for sender account %s, no transfer-failed email", eventID, event.GetSenderAccountId())
	}

	return finishTransferEvent(ctx, pool, eventID, sent)
}

// handleTransferRejected mails the sender only, and tells them less than
// it knows.
//
// The event carries triggered_rule, and this function reads it only to
// log it — buildTransferDeclinedEmail has no parameter that could carry
// it. Naming the rule or its threshold would hand a would-be fraudster
// the exact line to stay under, and unlike the UI an email is forwardable
// and permanent. Same conclusion the frontend reached in sprint 6. The
// recipient learns nothing at all, matching sprint 7's rule that nobody
// sees someone else's unsuccessful transfers.
func handleTransferRejected(ctx context.Context, pool *pgxpool.Pool, smtpAddr, smtpFrom string, event *eventsv1.TransferRejected) error {
	eventID := event.GetEventId()
	claimed, err := claimEvent(ctx, pool, eventID)
	if err != nil {
		return fmt.Errorf("claim event %s: %w", eventID, err)
	}
	if !claimed {
		log.Printf("notifications-svc: event %s already handled, skipping (redelivery)", eventID)
		return nil
	}

	sender, found, err := waitForContactByAccountID(ctx, pool, event.GetSenderAccountId())
	if err != nil {
		return fmt.Errorf("look up sender contact for event %s: %w", eventID, err)
	}
	sent := 0
	if found {
		// triggered_rule goes to the log, where operators need it, and
		// nowhere near the message body.
		log.Printf("notifications-svc: event %s: transfer %s rejected by rule %s, notifying sender without disclosing it", eventID, event.GetTransferId(), event.GetTriggeredRule())
		m := buildTransferDeclinedEmail(event.GetAmount(), event.GetTransferId(), eventTime(event.GetOccurredAt()))
		if err := sendEmailWithRetry(smtpAddr, smtpFrom, sender.Email, m); err != nil {
			return fmt.Errorf("send transfer-declined email for event %s: %w", eventID, err)
		}
		sent++
	} else {
		log.Printf("notifications-svc: event %s: no contact for sender account %s, no transfer-declined email", eventID, event.GetSenderAccountId())
	}

	return finishTransferEvent(ctx, pool, eventID, sent)
}

// handleDepositCredited mails the depositor once their Stripe-funded
// top-up has actually landed in their ledger balance — deliberately not
// any earlier: this event is published only when deposits.status reaches
// 'credited' (transfers-svc/deposit_reconcile.go), not at 'succeeded'
// (Stripe confirmed the charge, but the money isn't reflected in the
// user's balance yet). Same single-recipient shape as
// handleTransferFailed/handleTransferRejected — a deposit has no
// counterparty to also notify.
func handleDepositCredited(ctx context.Context, pool *pgxpool.Pool, smtpAddr, smtpFrom string, event *eventsv1.DepositCredited) error {
	eventID := event.GetEventId()
	claimed, err := claimEvent(ctx, pool, eventID)
	if err != nil {
		return fmt.Errorf("claim event %s: %w", eventID, err)
	}
	if !claimed {
		log.Printf("notifications-svc: event %s already handled, skipping (redelivery)", eventID)
		return nil
	}

	contact, found, err := waitForContactByAccountID(ctx, pool, event.GetAccountId())
	if err != nil {
		return fmt.Errorf("look up contact for event %s: %w", eventID, err)
	}
	sent := 0
	if found {
		m := buildDepositCreditedEmail(event.GetAmount(), event.GetDepositId(), eventTime(event.GetOccurredAt()))
		if err := sendEmailWithRetry(smtpAddr, smtpFrom, contact.Email, m); err != nil {
			return fmt.Errorf("send deposit-credited email for event %s: %w", eventID, err)
		}
		sent++
	} else {
		log.Printf("notifications-svc: event %s: no contact for account %s, no deposit-credited email", eventID, event.GetAccountId())
	}

	return finishTransferEvent(ctx, pool, eventID, sent)
}

// finishTransferEvent closes out a claimed event. 'skipped' when nothing
// was mailed — the honest record is "we decided not to send", not "we
// failed to send", since a failure would have returned an error before
// reaching here.
func finishTransferEvent(ctx context.Context, pool *pgxpool.Pool, eventID string, sent int) error {
	status := eventStatusSent
	if sent == 0 {
		status = eventStatusSkipped
	}
	if err := finishEvent(ctx, pool, eventID, status); err != nil {
		return fmt.Errorf("finish event %s as %s: %w", eventID, status, err)
	}
	return nil
}

// waitForContactByAccountID resolves an account_id to a contact, waiting
// a bounded time for the projection to catch up if the row isn't linked
// yet.
//
// Same root cause as handleAccountCreated's wait: three readers on three
// topics in this one process with no mutual ordering guarantee, so a
// transfer event can arrive before this service has consumed the
// AccountCreated that links its accounts — most acutely for a user who
// registers and transfers within a few seconds. Same reason for waiting
// in-process rather than returning an error, too: kafka-go does not
// redeliver within a running process, so returning here would simply drop
// the notification. Worst case is two waits back to back in
// handleTransferCompleted, ~6s of that goroutine, and only against a cold
// projection.
//
// A "not found" that outlasts the wait is terminal for the callers, not
// retried forever, and — deliberately — not an error: every account in
// this system is created by accounts-svc in response to a UserActivated,
// so a permanently unresolvable account means the projection is broken,
// not that this message is malformed. Returning it as an error would pull
// it into handleTransferMessageWithRetry's retry-then-DLQ path built for
// poison payloads, which is the wrong bucket for "the data isn't here
// yet, and may never be" — see that function's doc comment. A malformed
// UUID is a different case — Postgres rejects it (22P02) and it surfaces
// as an error here, which is the right treatment for genuinely corrupt
// data.
func waitForContactByAccountID(ctx context.Context, pool *pgxpool.Pool, accountID string) (contact, bool, error) {
	for attempt := 0; attempt < contactWaitAttempts; attempt++ {
		c, found, err := getContactByAccountID(ctx, pool, accountID)
		if err != nil {
			return contact{}, false, err
		}
		if found {
			return c, true, nil
		}
		time.Sleep(contactWaitDelay)
	}
	return contact{}, false, nil
}

// eventTime converts a proto timestamp for the email templates. IsValid
// covers both nil and out-of-range: AsTime() on a nil Timestamp returns
// 1970-01-01 rather than erroring, and a unix-epoch date in a customer's
// email is worse than no date line at all (which is what the zero time
// produces — see writeDateLine).
func eventTime(ts *timestamppb.Timestamp) time.Time {
	if !ts.IsValid() {
		return time.Time{}
	}
	return ts.AsTime().UTC()
}

// namedReader pairs a Reader with the topic name it reads, only so
// monitorConsumers can log something more useful than the reader's
// config object.
type namedReader struct {
	topic  string
	reader *kafka.Reader
}

// monitorConsumers periodically logs each reader's lag — how many
// messages on its topic it has not yet consumed — from
// Reader.Stats().Lag, computed internally on every fetch from the
// batch's high water mark. Not the deprecated Reader.Lag() method, which
// returns -1 in consumer-group mode (every reader here has a GroupID);
// Stats().Lag is populated regardless of mode. Without this,
// "notifications aren't arriving" is indistinguishable from the outside
// from "nobody transferred anything" — both look like silence in
// Mailpit. Lag is what tells them apart.
//
// Deliberately does NOT drive consumerHealth — see monitorKafkaHealth's
// doc comment for why Stats() turned out to be the wrong signal for that.
func monitorConsumers(ctx context.Context, readers []namedReader) {
	ticker := time.NewTicker(consumerLagLogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, nr := range readers {
				stats := nr.reader.Stats()
				log.Printf("notifications-svc: consumer lag: topic=%s lag=%d offset=%d", nr.topic, stats.Lag, stats.Offset)
			}
		}
	}
}

// monitorKafkaHealth periodically dials the Kafka brokers directly and
// updates consumerHealth from whether that succeeds.
//
// This used to be inferred from the three readers themselves — first
// from whether the last FetchMessage call errored (wrong: FetchMessage
// only returns once a message is ready or the fetch context is
// cancelled, so on an idle topic a recovered-but-quiet reader never gets
// a moment to flip the flag back), then from Reader.Stats().Errors
// compared between ticks, reasoning that kafka-go retries the broker
// connection in the background regardless of whether FetchMessage is
// blocked. Measured against a real outage (`docker compose stop kafka`),
// that second assumption was also wrong: repeatedly-logged "failed to
// dial" errors from FetchMessage did not move Stats().Errors at all
// after the first few, during startup — that counter tracks a narrower
// set of internal retry paths than a consumer-group reader's dial
// failures actually go through.
//
// A direct, independent probe sidesteps needing to know which internal
// counter happens to reflect which failure mode: it dials the brokers
// itself, on its own schedule, and the result IS the signal, not
// something inferred from reader internals that turned out not to mean
// what they looked like they meant.
func monitorKafkaHealth(ctx context.Context, brokers string, health *consumerHealth) {
	addrs := strings.Split(brokers, ",")
	ticker := time.NewTicker(kafkaHealthProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probeCtx, cancel := context.WithTimeout(ctx, kafkaHealthProbeTimeout)
			conn, err := kafka.DialContext(probeCtx, "tcp", addrs[0])
			cancel()
			health.unreachable.Store(err != nil)
			if err != nil {
				log.Printf("notifications-svc: kafka health probe: %v", err)
				continue
			}
			conn.Close()
		}
	}
}
