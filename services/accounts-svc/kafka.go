package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/proto"

	eventsv1 "neobank/proto/gen/go/events/v1"
)

// newKafkaReader constructs accounts-svc's consumer for the user.events
// topic. GroupID is set so that if this service is ever scaled to multiple
// replicas, Kafka's consumer-group protocol splits the topic's partitions
// across them (each message processed by exactly one replica) instead of
// every replica independently re-reading every message, which is what
// happens if GroupID is left empty.
func newKafkaReader(brokers, topic, groupID string) *kafka.Reader {
	return kafka.NewReader(kafka.ReaderConfig{
		Brokers: strings.Split(brokers, ","),
		Topic:   topic,
		GroupID: groupID,
	})
}

// runUserActivatedConsumer fetches UserActivated events and turns each one
// into an accounts row, one message at a time. It deliberately uses
// FetchMessage + an explicit CommitMessages (not the auto-committing
// ReadMessage) so the offset is only advanced after handleUserActivated
// has actually succeeded — a crash or DB error between fetch and commit
// leaves the message uncommitted and it will be redelivered, giving
// at-least-once processing. handleUserActivated is idempotent (two layers,
// see its doc comment), so a redelivery is a safe, logged no-op rather than
// a stuck or duplicate-creating consumer.
func runUserActivatedConsumer(ctx context.Context, reader *kafka.Reader, pool *pgxpool.Pool) {
	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			log.Printf("accounts-svc: failed to fetch message: %v", err)
			continue
		}

		var event eventsv1.UserActivated
		if err := proto.Unmarshal(msg.Value, &event); err != nil {
			log.Printf("accounts-svc: failed to unmarshal UserActivated at offset %d: %v", msg.Offset, err)
			// No amount of redelivery makes an unparseable payload
			// parseable — commit it so a poison message doesn't block
			// the partition forever.
			if cerr := reader.CommitMessages(ctx, msg); cerr != nil {
				log.Printf("accounts-svc: failed to commit offset for unparseable message: %v", cerr)
			}
			continue
		}

		if err := handleUserActivated(ctx, pool, &event); err != nil {
			log.Printf("accounts-svc: failed to handle UserActivated event %s for user %s: %v", event.GetEventId(), event.GetUserId(), err)
			continue // do not commit — message will be redelivered
		}

		if err := reader.CommitMessages(ctx, msg); err != nil {
			log.Printf("accounts-svc: failed to commit offset for user %s: %v", event.GetUserId(), err)
		}
	}
}

// handleUserActivated processes one UserActivated event idempotently. It is
// safe to call more than once for the same event_id — that's the whole
// point: Kafka redelivery (crash between the DB write and the offset
// commit, or ordinary at-least-once semantics) must be a safe, logged
// no-op, not a stuck or duplicate-creating consumer.
//
// Ordering is deliberate and load-bearing:
//  1. Check processed_events first (fast path): if event_id is already
//     recorded, the account for this user is already known to exist —
//     nothing further to do.
//  2. Only then attempt to create the account, via createAccountForUser's
//     own idempotent ON CONFLICT (user_id) path — which reports whether it
//     freshly created the row or found one already there.
//  3. Only after step 2 has confirmed the account row exists — either way —
//     mark processed_events. This is deliberately the LAST step, strictly
//     after the work it attests to has actually landed.
//
// Why step 3 must be last, not first: if processed_events were written
// before step 2 ran, a real, transient failure inside createAccountForUser
// (a dropped connection, not a user_id collision) would still leave the
// offset uncommitted so Kafka redelivers — but processed_events would
// already show it as done, so the redelivery would wrongly skip it, and
// that user would never get an account. Writing processed_events last
// closes that gap: a crash or error anywhere before it leaves
// processed_events empty, so redelivery always genuinely retries the real
// work.
//
// The two writes (accounts, processed_events) are deliberately not wrapped
// in one SQL transaction: doing so would require SAVEPOINT/ROLLBACK TO
// SAVEPOINT around every iteration of createAccountForUser's
// account-number-collision retry loop (Postgres aborts an entire
// transaction on any statement error, including an expected, retried
// collision). That complexity buys nothing here — this consumer is
// single-threaded and strictly sequential (FetchMessage is called one
// message at a time, no concurrent handleUserActivated calls within the
// process), and layer 1 (accounts.user_id UNIQUE + ON CONFLICT DO NOTHING)
// already absorbs every gap this two-step ordering could otherwise leave.
func handleUserActivated(ctx context.Context, pool *pgxpool.Pool, event *eventsv1.UserActivated) error {
	eventID := event.GetEventId()
	userID := event.GetUserId()

	if eventID == "" {
		// Defensive: only reachable from a producer that hasn't picked up
		// the event_id field yet, or a hand-crafted message. Skip the
		// processed_events fast-path/bookkeeping entirely rather than bind
		// an empty string to a UUID column (which Postgres would reject
		// with 22P02 on every redelivery — a new poison-message class).
		// Falls back to layer 1 alone for this one message.
		log.Printf("accounts-svc: UserActivated for user %s has no event_id, skipping processed_events bookkeeping", userID)
		_, err := createAccountForUser(ctx, pool, userID)
		return err
	}

	processed, err := isEventProcessed(ctx, pool, eventID)
	if err != nil {
		return fmt.Errorf("check processed_events for event %s: %w", eventID, err)
	}
	if processed {
		log.Printf("accounts-svc: event %s already processed, skipping (redelivery)", eventID)
		return nil
	}

	outcome, err := createAccountForUser(ctx, pool, userID)
	if err != nil {
		return fmt.Errorf("create account for user %s: %w", userID, err)
	}
	if outcome == accountAlreadyExists {
		log.Printf("accounts-svc: account for user %s already exists (redelivery of event %s), not recreating", userID, eventID)
	}

	if err := markEventProcessed(ctx, pool, eventID); err != nil {
		return fmt.Errorf("mark event %s processed: %w", eventID, err)
	}
	return nil
}
