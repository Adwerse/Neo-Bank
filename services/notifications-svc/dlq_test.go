package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/proto"

	"neobank/pkg/pgha"
	eventsv1 "neobank/proto/gen/go/events/v1"
)

// fakeDLQWriter is a kafkaMessageWriter that records what it was asked to
// publish instead of talking to a broker — same pattern as
// pkg/outbox.RelayBatch's tests use for KafkaMessageWriter.
type fakeDLQWriter struct {
	messages []kafka.Message
	err      error
}

func (f *fakeDLQWriter) WriteMessages(ctx context.Context, msgs ...kafka.Message) error {
	if f.err != nil {
		return f.err
	}
	f.messages = append(f.messages, msgs...)
	return nil
}

// TestTransferRetryDelay_ExponentialWithCap pins the backoff shape the DoD
// depends on: growing delays, not a fixed one, capped so a long losing
// streak still reaches the DLQ in bounded time.
func TestTransferRetryDelay_ExponentialWithCap(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 500 * time.Millisecond},
		{2, 1 * time.Second},
		{3, 2 * time.Second},
		{4, 4 * time.Second},
		{5, 8 * time.Second},        // would be 8s uncapped too — exact cap boundary
		{10, transferRetryMaxDelay}, // would be 256s uncapped; must be capped
	}
	for _, tt := range tests {
		if got := transferRetryDelay(tt.attempt); got != tt.want {
			t.Errorf("transferRetryDelay(%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

// TestSendToDLQ_PreservesPayloadAndRecordsReason is the audit-trail
// contract the DoD leans on ("the DLQ can be picked apart by hand"): the
// original key, value and headers must survive verbatim, with the
// failure reason and source coordinates added, not substituted.
func TestSendToDLQ_PreservesPayloadAndRecordsReason(t *testing.T) {
	writer := &fakeDLQWriter{}
	msg := kafka.Message{
		Partition: 3,
		Offset:    42,
		Key:       []byte("sender-account-123"),
		Value:     []byte("not a valid protobuf payload"),
		Headers:   []kafka.Header{{Key: eventTypeHeader, Value: []byte(eventTypeTransferCompleted)}},
	}
	cause := errors.New("unmarshal TransferCompleted at offset 42: proto: bad wire type")

	sendToDLQ(context.Background(), writer, "transfer.events.dlq", msg, eventTypeTransferCompleted, cause)

	if len(writer.messages) != 1 {
		t.Fatalf("got %d messages written to the DLQ, want 1", len(writer.messages))
	}
	got := writer.messages[0]

	if string(got.Key) != "sender-account-123" {
		t.Errorf("Key = %q, want the original key preserved", got.Key)
	}
	if string(got.Value) != "not a valid protobuf payload" {
		t.Errorf("Value = %q, want the original payload preserved verbatim", got.Value)
	}

	headers := map[string]string{}
	for _, h := range got.Headers {
		headers[h.Key] = string(h.Value)
	}
	if headers[eventTypeHeader] != eventTypeTransferCompleted {
		t.Errorf("original %s header lost, got %q", eventTypeHeader, headers[eventTypeHeader])
	}
	if headers["dlq_reason"] != cause.Error() {
		t.Errorf("dlq_reason = %q, want %q", headers["dlq_reason"], cause.Error())
	}
	if headers["dlq_source_partition"] != "3" {
		t.Errorf("dlq_source_partition = %q, want %q", headers["dlq_source_partition"], "3")
	}
	if headers["dlq_source_offset"] != "42" {
		t.Errorf("dlq_source_offset = %q, want %q", headers["dlq_source_offset"], "42")
	}
}

// TestSendToDLQ_WriterFailureDoesNotPanic covers a DLQ outage: sendToDLQ
// must log and return, not panic — the caller commits the source offset
// regardless (see handleTransferMessageWithRetry), so this failure mode
// already can't retry indefinitely; the point of this test is just that
// it degrades to a log line instead of taking the process down with it.
func TestSendToDLQ_WriterFailureDoesNotPanic(t *testing.T) {
	writer := &fakeDLQWriter{err: errors.New("broker unreachable")}
	msg := kafka.Message{Partition: 0, Offset: 1, Value: []byte("x")}

	sendToDLQ(context.Background(), writer, "transfer.events.dlq", msg, eventTypeTransferCompleted, errors.New("boom"))
}

// TestProcessTransferMessage_NoEventTypeHeader and its siblings below
// exercise the three ways a message can be poison before it ever reaches
// a handler — no header, an unrecognized header, and a payload that
// doesn't unmarshal. None of these touch pool (they return before any
// handler call), so passing nil is safe and keeps these DB-free, matching
// this file's other tests.
func TestProcessTransferMessage_NoEventTypeHeader(t *testing.T) {
	msg := kafka.Message{Partition: 1, Offset: 7}

	eventID, eventType, err := processTransferMessage(context.Background(), nil, "", "", msg)

	if err == nil {
		t.Fatal("expected an error for a message with no event_type header")
	}
	if eventID != "" {
		t.Errorf("eventID = %q, want \"\" — no event_id is recoverable without unmarshaling, which this deliberately avoids", eventID)
	}
	if eventType != "" {
		t.Errorf("eventType = %q, want \"\"", eventType)
	}
}

func TestProcessTransferMessage_UnknownEventType(t *testing.T) {
	msg := kafka.Message{
		Headers: []kafka.Header{{Key: eventTypeHeader, Value: []byte("TransferReversed")}},
	}

	eventID, eventType, err := processTransferMessage(context.Background(), nil, "", "", msg)

	if err == nil {
		t.Fatal("expected an error for an unrecognized event_type")
	}
	if eventID != "" {
		t.Errorf("eventID = %q, want \"\"", eventID)
	}
	if eventType != "TransferReversed" {
		t.Errorf("eventType = %q, want %q — the caller needs it for logging even though nothing routed", eventType, "TransferReversed")
	}
}

func TestProcessTransferMessage_UnmarshalFailure(t *testing.T) {
	msg := kafka.Message{
		Headers: []kafka.Header{{Key: eventTypeHeader, Value: []byte(eventTypeTransferCompleted)}},
		// 0xff as a tag byte decodes to a wire type of 7, which does not
		// exist (valid wire types are 0-5) — proto.Unmarshal rejects it
		// before ever touching a handler.
		Value: []byte{0xff, 0xff, 0xff},
	}

	eventID, eventType, err := processTransferMessage(context.Background(), nil, "", "", msg)

	if err == nil {
		t.Fatal("expected an unmarshal error for a corrupt payload")
	}
	if eventID != "" {
		t.Errorf("eventID = %q, want \"\" — the payload never parsed, so there is no event_id to recover", eventID)
	}
	if eventType != eventTypeTransferCompleted {
		t.Errorf("eventType = %q, want %q — the header is read before the failed unmarshal", eventType, eventTypeTransferCompleted)
	}
}

// TestProcessTransferMessage_PoisonErrorsAreNotUnavailable pins the
// boundary that decides between "retry this for up to five minutes" and
// "this is poison, dead-letter it".
//
// handleTransferMessageWithRetry asks pgha.IsUnavailable about every
// failure. A poison error wrongly classified as unavailable would stall
// the whole partition for transferUnavailableBudget before reaching the
// DLQ it was always going to reach — turning a fast, correct outcome into
// a five-minute pause per bad message. These are exactly the three
// failures processTransferMessage can produce without touching Postgres,
// so none of them may ever look like a database outage.
func TestProcessTransferMessage_PoisonErrorsAreNotUnavailable(t *testing.T) {
	tests := []struct {
		name string
		msg  kafka.Message
	}{
		{
			name: "no event_type header",
			msg:  kafka.Message{Partition: 1, Offset: 7},
		},
		{
			name: "unknown event_type",
			msg: kafka.Message{
				Headers: []kafka.Header{{Key: eventTypeHeader, Value: []byte("TransferReversed")}},
			},
		},
		{
			name: "corrupt payload",
			msg: kafka.Message{
				Headers: []kafka.Header{{Key: eventTypeHeader, Value: []byte(eventTypeTransferCompleted)}},
				Value:   []byte{0xff, 0xff, 0xff},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := processTransferMessage(context.Background(), nil, "", "", tt.msg)
			if err == nil {
				t.Fatal("expected an error")
			}
			if pgha.IsUnavailable(err) {
				t.Errorf("pgha.IsUnavailable(%v) = true, want false — a poison message must reach the DLQ promptly, not sit out the unavailable budget first", err)
			}
		})
	}
}

// TestProcessTransferMessage_UnreachableDatabaseIsUnavailable is the other
// half of that boundary, and the one a failover actually exercises: when
// the failure IS Postgres being unreachable, it must classify as
// unavailable so the retry comes out of transferUnavailableBudget rather
// than the five-attempt poison ladder.
//
// Before this classification existed, a failover — which lasts longer
// than the ladder's ~7.5s — exhausted all five attempts and dead-lettered
// a burst of perfectly good transfer notifications.
//
// The pool points at a port nothing listens on, which is the same class
// of error (a failed connect) the real thing produces when the leader is
// gone.
func TestProcessTransferMessage_UnreachableDatabaseIsUnavailable(t *testing.T) {
	pool, err := pgha.NewPool(context.Background(), "postgres://neobank:pw@127.0.0.1:1/neobank?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("build pool: %v", err)
	}
	defer pool.Close()

	// A well-formed TransferCompleted: everything before the first
	// database access must succeed, so that the error under test is the
	// database one and not a parsing failure.
	payload, err := proto.Marshal(&eventsv1.TransferCompleted{
		EventId:         "6f1b7b3c-6b1a-4a5e-9a3d-2f4c8e0d1a2b",
		SenderAccountId: "9a1c2d3e-4f50-4a6b-8c7d-0e1f2a3b4c5d",
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}

	_, _, err = processTransferMessage(context.Background(), pool, "127.0.0.1:1", "noreply@neobank.local", kafka.Message{
		Headers: []kafka.Header{{Key: eventTypeHeader, Value: []byte(eventTypeTransferCompleted)}},
		Value:   payload,
	})
	if err == nil {
		t.Fatal("expected an error with no database reachable")
	}
	if !pgha.IsUnavailable(err) {
		t.Errorf("pgha.IsUnavailable(%v) = false, want true — a failover would burn the poison-retry budget and dead-letter good notifications", err)
	}
}
