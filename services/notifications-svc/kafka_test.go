package main

import (
	"testing"

	"github.com/segmentio/kafka-go"
)

// TestEventTypeOf covers the only thing standing between this consumer
// and silently decoding a TransferFailed as a TransferCompleted —
// protobuf will do that without an error, so a header lookup that
// misfires does not fail loudly anywhere else.
func TestEventTypeOf(t *testing.T) {
	tests := []struct {
		name    string
		headers []kafka.Header
		want    string
	}{
		{
			name:    "no headers at all (published before the discriminator existed)",
			headers: nil,
			want:    "",
		},
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
			name:    "present but empty — indistinguishable from absent, and both mean 'cannot route'",
			headers: []kafka.Header{{Key: "event_type", Value: []byte("")}},
			want:    "",
		},
		{
			name:    "other headers only",
			headers: []kafka.Header{{Key: "trace_id", Value: []byte("abc")}},
			want:    "",
		},
		{
			name:    "wrong case must not match — Kafka header keys are opaque bytes",
			headers: []kafka.Header{{Key: "Event_Type", Value: []byte("TransferCompleted")}},
			want:    "",
		},
		{
			name: "duplicate keys: first wins",
			headers: []kafka.Header{
				{Key: "event_type", Value: []byte("TransferCompleted")},
				{Key: "event_type", Value: []byte("TransferFailed")},
			},
			want: "TransferCompleted",
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

// TestEventTypeHeader_MatchesProducer pins this module's copy of the
// header key. pkg/outbox holds the other copy and has the mirror-image
// test; neither module imports the other (see eventTypeHeader for why),
// so these two assertions are the whole of the contract.
func TestEventTypeHeader_MatchesProducer(t *testing.T) {
	if eventTypeHeader != "event_type" {
		t.Errorf("eventTypeHeader = %q, want %q — must match pkg/outbox.HeaderEventType", eventTypeHeader, "event_type")
	}
}

// TestEventTypeConstants_MatchOutboxEventTypes pins the routing values
// against the event_type strings transfers-svc writes into its outbox
// rows (services/transfers-svc/transfer.go, deposit.go). A drift here
// routes every transfer or deposit event to the "unknown event_type"
// branch, which commits and logs — i.e. emails stop with no error
// anywhere.
func TestEventTypeConstants_MatchOutboxEventTypes(t *testing.T) {
	for _, tt := range []struct{ got, want string }{
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
