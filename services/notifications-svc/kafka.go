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

	eventsv1 "neobank/proto/gen/go/events/v1"
)

const notificationsConsumerGroup = "notifications-svc"

// accountLinkRetryAttempts/Delay bound handleAccountCreated's in-process
// wait for the corresponding UserActivated to land — see that function's
// doc comment for why this can't just rely on Kafka redelivery.
const accountLinkRetryAttempts = 15
const accountLinkRetryDelay = 200 * time.Millisecond

// newKafkaReader constructs one of notifications-svc's consumers (there
// are two — user.events and account.events — each with its own reader and
// its own goroutine, since kafka-go's Reader subscribes to exactly one
// topic; both use the same GroupID, which is fine, Kafka tracks committed
// offsets per topic-partition within a group regardless of how many
// topics that group happens to span).
//
// StartOffset is set explicitly to FirstOffset (earliest), not left at
// kafka-go's implicit default: this consumer group is subscribing for the
// first time long after UserActivated has been published (since sprint
// 2) — without an explicit earliest start, a brand-new group could begin
// reading from the tail of the topic, and every user activated before
// notifications-svc existed would never make it into user_contacts.
// StartOffset only takes effect the first time this GroupID has no
// committed offset yet on a given partition; every read after that
// resumes from the committed offset regardless of this setting, so it's
// safe to leave in place permanently rather than removing it after the
// first deploy. It's also necessary but not sufficient on its own — see
// README's "Kafka: offset reset и retention" section for why user.events
// was also switched to log compaction.
func newKafkaReader(brokers, topic string) *kafka.Reader {
	return kafka.NewReader(kafka.ReaderConfig{
		Brokers:     strings.Split(brokers, ","),
		Topic:       topic,
		GroupID:     notificationsConsumerGroup,
		StartOffset: kafka.FirstOffset,
	})
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
			continue
		}

		var event eventsv1.UserActivated
		if err := proto.Unmarshal(msg.Value, &event); err != nil {
			log.Printf("notifications-svc: user.events: failed to unmarshal UserActivated at offset %d: %v", msg.Offset, err)
			// No amount of redelivery makes an unparseable payload
			// parseable — commit it so a poison message doesn't block
			// the partition forever.
			if cerr := reader.CommitMessages(ctx, msg); cerr != nil {
				log.Printf("notifications-svc: user.events: failed to commit offset for unparseable message: %v", cerr)
			}
			continue
		}

		if err := handleUserActivated(ctx, pool, &event); err != nil {
			log.Printf("notifications-svc: failed to handle UserActivated event %s for user %s: %v", event.GetEventId(), event.GetUserId(), err)
			continue // do not commit — message will be redelivered
		}

		if err := reader.CommitMessages(ctx, msg); err != nil {
			log.Printf("notifications-svc: user.events: failed to commit offset for user %s: %v", event.GetUserId(), err)
		}
	}
}

func runAccountCreatedConsumer(ctx context.Context, reader *kafka.Reader, pool *pgxpool.Pool) {
	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			log.Printf("notifications-svc: account.events: failed to fetch message: %v", err)
			continue
		}

		var event eventsv1.AccountCreated
		if err := proto.Unmarshal(msg.Value, &event); err != nil {
			log.Printf("notifications-svc: account.events: failed to unmarshal AccountCreated at offset %d: %v", msg.Offset, err)
			if cerr := reader.CommitMessages(ctx, msg); cerr != nil {
				log.Printf("notifications-svc: account.events: failed to commit offset for unparseable message: %v", cerr)
			}
			continue
		}

		if err := handleAccountCreated(ctx, pool, &event); err != nil {
			log.Printf("notifications-svc: failed to handle AccountCreated event %s for user %s: %v", event.GetEventId(), event.GetUserId(), err)
			continue
		}

		if err := reader.CommitMessages(ctx, msg); err != nil {
			log.Printf("notifications-svc: account.events: failed to commit offset for user %s: %v", event.GetUserId(), err)
		}
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

	// "skipped": this sprint never sends a notification for UserActivated,
	// it only maintains the projection — see the processed_events
	// migration's doc comment for what 'sent'/'processing' are reserved for.
	if err := markEventProcessed(ctx, pool, eventID, "skipped"); err != nil {
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
	for attempt := 0; attempt < accountLinkRetryAttempts; attempt++ {
		linked, err = updateUserContactAccountID(ctx, pool, event.GetUserId(), event.GetAccountId())
		if err != nil {
			return fmt.Errorf("update account_id for user %s: %w", event.GetUserId(), err)
		}
		if linked {
			break
		}
		time.Sleep(accountLinkRetryDelay)
	}
	if !linked {
		return fmt.Errorf("no user_contacts row for user %s after %d attempts (event %s, account %s): giving up for this pass", event.GetUserId(), accountLinkRetryAttempts, eventID, event.GetAccountId())
	}

	if err := markEventProcessed(ctx, pool, eventID, "skipped"); err != nil {
		return fmt.Errorf("mark event %s processed: %w", eventID, err)
	}
	return nil
}
