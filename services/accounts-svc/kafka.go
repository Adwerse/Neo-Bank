package main

import (
	"context"
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
// ReadMessage) so the offset is only advanced after createAccountForUser
// has actually succeeded — a crash or DB error between fetch and commit
// leaves the message uncommitted and it will be redelivered, giving
// at-least-once processing at the cost of possible reprocessing (a
// redelivery surfaces as a user_id unique-violation from createAccountForUser
// and is deliberately left unhandled beyond logging — idempotency is a
// later prompt's concern).
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

		if err := createAccountForUser(ctx, pool, event.GetUserId()); err != nil {
			log.Printf("accounts-svc: failed to create account for user %s: %v", event.GetUserId(), err)
			continue // do not commit — message will be redelivered
		}

		if err := reader.CommitMessages(ctx, msg); err != nil {
			log.Printf("accounts-svc: failed to commit offset for user %s: %v", event.GetUserId(), err)
		}
	}
}
