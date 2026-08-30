package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

// errInvalidCursor is returned both by decodeCursor (structurally malformed
// input: not base64, not JSON, missing/zero fields) and by
// getTransferHistoryPage (structurally valid but semantically bogus — e.g.
// an id that isn't a real UUID, caught via Postgres's own 22P02). The
// handler treats both the same way: 400, not a silent "start over." A
// cursor is an opaque continuation token round-tripped from a prior
// response, not a casual default like the old offset — silently falling
// back to page 1 would return 200 with a different (wrong) page and mask
// that from the client.
var errInvalidCursor = errors.New("invalid_cursor")

// pageCursor identifies the last row of a previously returned page, by the
// same (created_at, id) tie-break getTransferHistoryPage's ORDER BY uses.
type pageCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

// encodeCursor never errors: pageCursor only ever holds a time.Time and a
// string, both of which always marshal successfully.
func encodeCursor(c pageCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (pageCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return pageCursor{}, errInvalidCursor
	}
	var c pageCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return pageCursor{}, errInvalidCursor
	}
	if c.ID == "" || c.CreatedAt.IsZero() {
		return pageCursor{}, errInvalidCursor
	}
	return c, nil
}
