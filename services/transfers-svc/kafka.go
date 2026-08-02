package main

import (
	"strings"

	"github.com/segmentio/kafka-go"
)

// newKafkaWriter constructs transfers-svc's producer for the transfer.events
// topic — same shape as auth-svc's newKafkaWriter (services/auth-svc/kafka.go),
// duplicated rather than shared per this repo's per-service-module
// convention (no shared package between services).
//
// Balancer is set explicitly: the zero-value Writer's default balancer does
// not take the message key into account when picking a partition. Every
// event the outbox relay publishes is keyed by partition_key (a transfer's
// sender_account_id) so a given account's events land on, and are read back
// in order from, a single partition.
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
