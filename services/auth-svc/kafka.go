package main

import (
	"strings"

	"github.com/segmentio/kafka-go"
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
// event the outbox relay publishes here is keyed by partition_key (a
// UserActivated event's user_id) so a given user's events land on, and are
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
