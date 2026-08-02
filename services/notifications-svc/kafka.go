package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "neobank/proto/gen/go/events/v1"
)

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

// eventTypeHeader is the Kafka message header carrying the event's type.
// It duplicates pkg/outbox's HeaderEventType literal rather than
// importing that module: what's needed here is one string, and
// pkg/outbox is the PUBLISHING side (write into an outbox table in the
// same transaction, then relay it). notifications-svc owns no outbox
// table and publishes nothing; a pure consumer depending on the
// producers' library would invert the layering and mislead the next
// reader into thinking this service emits events. pkg/outbox's
// TestHeaderEventType_IsWireContract pins the literal there so a rename
// fails loudly on the producing side instead of silently producing a
// topic nobody can route.
const eventTypeHeader = "event_type"

// The event_type values transfers-svc writes into its outbox rows, which
// the relay copies verbatim onto the header.
const (
	eventTypeTransferCompleted = "TransferCompleted"
	eventTypeTransferFailed    = "TransferFailed"
	eventTypeTransferRejected  = "TransferRejected"
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

// runUserActivatedConsumer and runAccountCreatedConsumer follow the same
// at-least-once shape as accounts-svc's runUserActivatedConsumer:
// FetchMessage + explicit CommitMessages, only after the handler
// succeeds, so a crash or DB error between fetch and commit leaves the
// message uncommitted for redelivery rather than silently skipped.
func runUserActivatedConsumer(ctx context.Context, reader *kafka.Reader, pool *pgxpool.Pool) {
	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			log.Printf("notifications-svc: user.events: failed to fetch message: %v", err)
			time.Sleep(fetchErrorBackoff)
			continue
		}

		var event eventsv1.UserActivated
		if err := proto.Unmarshal(msg.Value, &event); err != nil {
			log.Printf("notifications-svc: user.events: failed to unmarshal UserActivated at offset %d: %v", msg.Offset, err)
			// No amount of redelivery makes an unparseable payload
			// parseable — commit it so a poison message doesn't block
			// the partition forever.
			commitMessage(ctx, reader, msg, "user.events")
			continue
		}

		if err := handleUserActivated(ctx, pool, &event); err != nil {
			log.Printf("notifications-svc: failed to handle UserActivated event %s for user %s: %v", event.GetEventId(), event.GetUserId(), err)
			continue // do not commit — message will be redelivered
		}

		commitMessage(ctx, reader, msg, "user.events")
	}
}

func runAccountCreatedConsumer(ctx context.Context, reader *kafka.Reader, pool *pgxpool.Pool) {
	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			log.Printf("notifications-svc: account.events: failed to fetch message: %v", err)
			time.Sleep(fetchErrorBackoff)
			continue
		}

		var event eventsv1.AccountCreated
		if err := proto.Unmarshal(msg.Value, &event); err != nil {
			log.Printf("notifications-svc: account.events: failed to unmarshal AccountCreated at offset %d: %v", msg.Offset, err)
			commitMessage(ctx, reader, msg, "account.events")
			continue
		}

		if err := handleAccountCreated(ctx, pool, &event); err != nil {
			log.Printf("notifications-svc: failed to handle AccountCreated event %s for user %s: %v", event.GetEventId(), event.GetUserId(), err)
			continue
		}

		commitMessage(ctx, reader, msg, "account.events")
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
// handlers reach outside the process. It differs from the two projection
// loops in two ways.
//
// First, it must decide what it is holding before it can unmarshal.
// transfer.events carries three message types, and protobuf's wire
// format cannot tell them apart: TransferCompleted/Failed/Rejected share
// field numbers and types for fields 1-5, and field 6 is a string in all
// three, so decoding any one of them as any other SUCCEEDS and quietly
// files a failure reason under LedgerTransactionId. The event_type header
// the outbox relay stamps is the only discriminator; see eventTypeHeader.
//
// Second, "don't commit on error" buys much less here than its
// familiarity suggests. kafka-go's Reader has no per-message redelivery
// inside a running process — FetchMessage always advances — so leaving an
// offset uncommitted only helps if the process restarts before any later
// offset on that partition commits. It is still the right default (a
// broken SMTP server usually breaks the next message too, so nothing
// later commits past it and a restart genuinely does replay), but the
// mechanism actually carrying a transient outage is sendEmailWithRetry.
func runTransferEventsConsumer(ctx context.Context, reader *kafka.Reader, pool *pgxpool.Pool, smtpAddr, smtpFrom string) {
	const topic = "transfer.events"
	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			log.Printf("notifications-svc: %s: failed to fetch message: %v", topic, err)
			time.Sleep(fetchErrorBackoff)
			continue
		}

		eventType := eventTypeOf(msg)
		var handleErr error

		switch eventType {
		case eventTypeTransferCompleted:
			var event eventsv1.TransferCompleted
			if err := proto.Unmarshal(msg.Value, &event); err != nil {
				log.Printf("notifications-svc: %s: failed to unmarshal %s at offset %d: %v", topic, eventType, msg.Offset, err)
				commitMessage(ctx, reader, msg, topic)
				continue
			}
			handleErr = handleTransferCompleted(ctx, pool, smtpAddr, smtpFrom, &event)

		case eventTypeTransferFailed:
			var event eventsv1.TransferFailed
			if err := proto.Unmarshal(msg.Value, &event); err != nil {
				log.Printf("notifications-svc: %s: failed to unmarshal %s at offset %d: %v", topic, eventType, msg.Offset, err)
				commitMessage(ctx, reader, msg, topic)
				continue
			}
			handleErr = handleTransferFailed(ctx, pool, smtpAddr, smtpFrom, &event)

		case eventTypeTransferRejected:
			var event eventsv1.TransferRejected
			if err := proto.Unmarshal(msg.Value, &event); err != nil {
				log.Printf("notifications-svc: %s: failed to unmarshal %s at offset %d: %v", topic, eventType, msg.Offset, err)
				commitMessage(ctx, reader, msg, topic)
				continue
			}
			handleErr = handleTransferRejected(ctx, pool, smtpAddr, smtpFrom, &event)

		case "":
			// Published before the relay started stamping the header. An
			// old message will never grow one, so this is structurally a
			// poison message: commit rather than wedge the partition on a
			// notification nobody is still waiting for. Transitional — it
			// disappears once the topic ages past retention.
			//
			// Deliberately NOT unmarshaled as TransferCompleted just to
			// recover an event_id for this log line, tempting as that is
			// with fields 1-5 identical: that is exactly the accidental
			// cross-decode the header exists to prevent. Partition and
			// offset are what an operator needs to inspect it anyway.
			log.Printf("notifications-svc: %s: message at partition %d offset %d has no %q header (published before the discriminator existed), skipping", topic, msg.Partition, msg.Offset, eventTypeHeader)
			commitMessage(ctx, reader, msg, topic)
			continue

		default:
			// Forward compatibility: a type a later sprint adds must not
			// brick this partition for an older binary.
			log.Printf("notifications-svc: %s: unknown event_type %q at partition %d offset %d, skipping", topic, eventType, msg.Partition, msg.Offset)
			commitMessage(ctx, reader, msg, topic)
			continue
		}

		if handleErr != nil {
			log.Printf("notifications-svc: %s: failed to handle %s at offset %d: %v", topic, eventType, msg.Offset, handleErr)
			continue // do not commit
		}
		commitMessage(ctx, reader, msg, topic)
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
// retried forever: every account in this system is created by accounts-svc
// in response to a UserActivated, so a permanently unresolvable account
// means the projection is broken, and parking the consumer on a message it
// can never satisfy would cost the notifications behind it for nothing. A
// malformed UUID is a different case — Postgres rejects it (22P02) and it
// surfaces as an error here, which is the right treatment for genuinely
// corrupt data.
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
