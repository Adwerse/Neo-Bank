// Package outbox implements the transactional outbox pattern shared by
// every service in this repo that needs to publish a Kafka event as a
// side effect of a Postgres write: write the event into an "outbox" table
// in the SAME transaction as the write it accompanies, then let a
// separate relay (RunRelay) deliver it to Kafka later. Because the event
// row and the write it describes commit or roll back together, a crash
// (or Kafka being unreachable) between "the write happened" and "Kafka
// knows about it" can never silently drop the event — it just sits
// unpublished until the relay catches up.
//
// This package owns no migrations of its own: each service creates its
// own outbox table (its own name, since services share one physical
// Postgres database and table names must not collide) with this shape:
//
//	CREATE TABLE <table> (
//	    id BIGSERIAL PRIMARY KEY,
//	    event_id UUID NOT NULL UNIQUE,
//	    event_type TEXT NOT NULL,
//	    partition_key TEXT NOT NULL,
//	    payload BYTEA NOT NULL,
//	    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
//	    published_at TIMESTAMPTZ
//	);
//	CREATE INDEX ON <table> (published_at, id) WHERE published_at IS NULL;
package outbox

import (
	"context"
	crand "crypto/rand"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// GenerateEventID returns a random UUIDv4 (RFC 4122), hand-rolled from
// crypto/rand rather than adding google/uuid as a dependency — this
// repo's established convention (see auth-svc's register.go/login.go
// generators). Centralized here because an outbox event's id has to be
// known before the row exists: it's embedded in the event payload itself,
// which is marshaled before InsertEvent is called, so it can't come from
// a database-generated default the way most other primary keys in this
// repo do.
func GenerateEventID() (string, error) {
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx (RFC 4122)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// InsertEvent writes one outbox row as part of the caller's own
// transaction tx — it never begins or commits anything itself, so it
// shares the atomicity of whatever write the caller already made within
// tx: both persist or neither does. published_at is left NULL; RunRelay
// is what eventually publishes the row to Kafka and stamps it.
//
// table is the caller's outbox table name, injected directly into the
// SQL text since Postgres doesn't allow binding a table name as a query
// parameter. Every call site in this repo passes a hardcoded literal
// ("outbox", "auth_outbox", ...), never a value derived from external
// input.
func InsertEvent(ctx context.Context, tx pgx.Tx, table, eventID, eventType, partitionKey string, payload []byte) error {
	_, err := tx.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %s (event_id, event_type, partition_key, payload) VALUES ($1, $2, $3, $4)`, table),
		eventID, eventType, partitionKey, payload,
	)
	return err
}
