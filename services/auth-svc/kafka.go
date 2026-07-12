package main

import (
	"context"
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
	payload, err := proto.Marshal(&eventsv1.UserActivated{
		UserId:     userID,
		Email:      email,
		OccurredAt: timestamppb.New(time.Now()),
	})
	if err != nil {
		return err
	}

	return w.WriteMessages(ctx, kafka.Message{
		Key:   []byte(userID),
		Value: payload,
	})
}
