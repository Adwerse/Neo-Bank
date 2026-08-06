package main

import (
	"testing"

	"github.com/segmentio/kafka-go"
)

// TestEventTypeOf mirrors notifications-svc's own test of the identical
// function — both consumers face the same wire-ambiguity problem on
// transfer.events (see pkg/outbox/relay.go's HeaderEventType doc
// comment), so both pin the same header-parsing edge cases.
func TestEventTypeOf(t *testing.T) {
	tests := []struct {
		name    string
		headers []kafka.Header
		want    string
	}{
		{name: "no headers at all", headers: nil, want: ""},
		{
			name:    "the header is present",
			headers: []kafka.Header{{Key: "event_type", Value: []byte("TransferCompleted")}},
			want:    "TransferCompleted",
		},
		{
			name: "present among others",
			headers: []kafka.Header{
				{Key: "trace_id", Value: []byte("abc")},
				{Key: "event_type", Value: []byte("TransferRejected")},
			},
			want: "TransferRejected",
		},
		{
			name:    "present but empty — indistinguishable from absent",
			headers: []kafka.Header{{Key: "event_type", Value: []byte("")}},
			want:    "",
		},
		{
			name:    "wrong case must not match — Kafka header keys are opaque bytes",
			headers: []kafka.Header{{Key: "Event_Type", Value: []byte("TransferCompleted")}},
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eventTypeOf(kafka.Message{Headers: tt.headers})
			if got != tt.want {
				t.Errorf("eventTypeOf() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestEventTypeConstants_MatchOutboxEventTypes pins the routing literals
// against the event_type strings transfers-svc/accounts-svc actually
// write into their outbox rows. A drift here silently routes every
// transfer/deposit/account event to the "unknown, skip" branch — no
// error anywhere, WS pushes just stop.
func TestEventTypeConstants_MatchOutboxEventTypes(t *testing.T) {
	for _, tt := range []struct{ got, want string }{
		{eventTypeAccountCreated, "AccountCreated"},
		{eventTypeTransferCompleted, "TransferCompleted"},
		{eventTypeTransferFailed, "TransferFailed"},
		{eventTypeTransferRejected, "TransferRejected"},
		{eventTypeDepositCredited, "DepositCredited"},
	} {
		if tt.got != tt.want {
			t.Errorf("event type constant = %q, want %q", tt.got, tt.want)
		}
	}
}

func TestEventTypeHeader_MatchesProducer(t *testing.T) {
	if eventTypeHeader != "event_type" {
		t.Errorf("eventTypeHeader = %q, want %q — must match pkg/outbox.HeaderEventType", eventTypeHeader, "event_type")
	}
}

// TestNewInstanceConsumerGroup_UniquePerCall is the core premise the
// whole multi-instance fan-out design rests on: if two calls ever
// produced the same group id, Kafka would split transfer.events'
// partitions between them instead of giving each instance every event,
// silently reintroducing the "wrong instance has the connection" bug
// this design exists to avoid.
func TestNewInstanceConsumerGroup_UniquePerCall(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 20; i++ {
		group := newInstanceConsumerGroup()
		if group == "" {
			t.Fatal("newInstanceConsumerGroup() returned an empty string")
		}
		if seen[group] {
			t.Fatalf("newInstanceConsumerGroup() produced a duplicate: %q", group)
		}
		seen[group] = true
	}
}
