package main

import (
	"context"
	crand "crypto/rand"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// generateEventID returns a random UUIDv4 (RFC 4122), hand-rolled from
// crypto/rand — same convention as auth-svc's generateEventID
// (services/auth-svc/kafka.go). No shared package exists between service
// modules in this repo, so this is duplicated rather than imported.
func generateEventID() (string, error) {
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx (RFC 4122)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// insertOutboxEvent writes one outbox row as part of the caller's own
// transaction tx — it never begins or commits anything itself, so it
// shares the atomicity of whatever status UPDATE the caller already ran:
// both persist or neither does. published_at is left NULL; a separate
// relay process (not implemented yet) is what eventually publishes this
// row to Kafka and stamps published_at.
func insertOutboxEvent(ctx context.Context, tx pgx.Tx, eventID, eventType, partitionKey string, payload []byte) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO outbox (event_id, event_type, partition_key, payload) VALUES ($1, $2, $3, $4)`,
		eventID, eventType, partitionKey, payload,
	)
	return err
}
