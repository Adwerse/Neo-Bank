package main

import (
	"context"
	crand "crypto/rand"
	"fmt"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	eventsv1 "neobank/proto/gen/go/events/v1"
)

// newKafkaWriter constructs auth-svc's producer for the user.events topic.
// Like pgxpool.New and redis.NewClient in main.go, this does not dial the
// broker: kafka-go's Writer connects lazily on the first WriteMessages call
// and retries/reconnects internally, so starting (or continuing to run)
// while Kafka is unreachable is not a startup failure here — no extra
// health-check/connect/retry code is needed to get that property.
//
// Balancer is set explicitly: the zero-value Writer's default balancer does
// not take the message key into account when picking a partition. Every
// event here is keyed by user_id so a given user's events land on, and are
// read back in order from, a single partition — leaving Balancer unset
// would silently break that guarantee (no error, messages just scatter
// across partitions key-blind).
//
// AllowAutoTopicCreation must be set true client-side even though the
// broker already has KAFKA_CFG_AUTO_CREATE_TOPICS_ENABLE=true — kafka-go's
// writer does not request auto-creation on an unknown topic unless this
// field is also set, regardless of the broker's own config.
func newKafkaWriter(brokers, topic string) *kafka.Writer {
	return &kafka.Writer{
		Addr:                   kafka.TCP(strings.Split(brokers, ",")...),
		Topic:                  topic,
		Balancer:               &kafka.Hash{},
		AllowAutoTopicCreation: true,
	}
}

// publishUserActivated marshals and publishes a UserActivated event for
// (userID, email), keyed by user_id.
//
// TODO: this runs after the Postgres transaction that flips the user to
// status='active' has already committed (see verifyEmailCode) — it is not
// part of that transaction. A crash, or Kafka being unreachable longer than
// the writer's retry budget, between the commit and this call means the
// activation can succeed with no event ever published. An outbox table
// (write the event row in the same DB transaction, relay it to Kafka from a
// separate process) would close that gap; deliberately out of scope here.
func publishUserActivated(ctx context.Context, w *kafka.Writer, userID, email string) error {
	eventID, err := generateEventID()
	if err != nil {
		return err
	}

	payload, err := proto.Marshal(&eventsv1.UserActivated{
		UserId:     userID,
		Email:      email,
		OccurredAt: timestamppb.New(time.Now()),
		EventId:    eventID,
	})
	if err != nil {
		return err
	}

	return w.WriteMessages(ctx, kafka.Message{
		Key:   []byte(userID),
		Value: payload,
	})
}

// generateEventID returns a random UUIDv4 (RFC 4122), hand-rolled from
// crypto/rand like this repo's other small random-value generators
// (generateCode and generateRefreshToken in register.go/login.go,
// generateAccountNumber in accounts-svc/accounts.go) rather than adding
// google/uuid as a new dependency. This is the first place in the repo a
// UUID is minted in Go rather than by Postgres's gen_random_uuid() — the
// event needs its id before it ever reaches Kafka, so it can't come from
// the database.
//
// Trade-off, stated rather than silently decided: a well-tested library
// removes the small risk of getting the version/variant bit-twiddling
// wrong; this repo has consistently chosen to hand-roll this class of
// problem instead, so this continues that convention.
func generateEventID() (string, error) {
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx (RFC 4122)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
