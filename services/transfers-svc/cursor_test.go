package main

import (
	"errors"
	"testing"
	"time"
)

func TestCursor_RoundTrip(t *testing.T) {
	want := pageCursor{CreatedAt: time.Now().UTC().Truncate(time.Microsecond), ID: "b6f8a3d0-2b1a-4e9a-9c3a-1f0b2c3d4e5f"}

	got, err := decodeCursor(encodeCursor(want))
	if err != nil {
		t.Fatalf("decodeCursor(encodeCursor(want)): unexpected error: %v", err)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
	}
	if got.ID != want.ID {
		t.Errorf("ID = %q, want %q", got.ID, want.ID)
	}
}

func TestDecodeCursor_Invalid(t *testing.T) {
	cases := map[string]string{
		"not base64":          "not-valid-base64!!",
		"base64 but not json": "bm90IGpzb24=", // "not json"
		"empty id":            encodeCursor(pageCursor{CreatedAt: time.Now(), ID: ""}),
		"zero time":           encodeCursor(pageCursor{CreatedAt: time.Time{}, ID: "some-id"}),
		"empty string":        "",
	}
	for name, cursor := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeCursor(cursor); !errors.Is(err, errInvalidCursor) {
				t.Errorf("decodeCursor(%q) error = %v, want errInvalidCursor", cursor, err)
			}
		})
	}
}
