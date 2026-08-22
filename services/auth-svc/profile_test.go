package main

import (
	"context"
	crand "crypto/rand"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"

	eventsv1 "neobank/proto/gen/go/events/v1"
)

// TestValidateDisplayName_Valid pins the shapes that must be accepted —
// including non-Latin scripts, since this field must not be Latin-only.
func TestValidateDisplayName_Valid(t *testing.T) {
	valid := []string{
		"Alice",
		"Jean-Luc",
		"O'Brien",
		"日本語",
		"Александр",
		strings.Repeat("a", maxDisplayNameLength), // exactly at the limit
	}
	for _, name := range valid {
		if err := validateDisplayName(name); err != nil {
			t.Errorf("validateDisplayName(%q) = %v, want nil", name, err)
		}
	}
}

// TestValidateDisplayName_ControlCharacters is the DoD's explicit
// requirement: a name built from control characters must be rejected.
func TestValidateDisplayName_ControlCharacters(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"newline", "Alice\nBob"},
		{"tab", "Alice\tBob"},
		{"carriage return", "Alice\rBob"},
		{"null byte", "Alice\x00Bob"},
		{"bell", "Alice\aBob"},
		{"C1 control", "AliceBob"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateDisplayName(tt.value); err == nil {
				t.Errorf("validateDisplayName(%q) = nil, want an error", tt.value)
			}
		})
	}
}

// TestValidateDisplayName_BidiControlCharacters guards against the
// visual-spoofing vector the task calls out by name: a bidi override
// character can make a stored name render as a different string than its
// actual byte sequence.
func TestValidateDisplayName_BidiControlCharacters(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"RLO", "Alice" + string(rune(0x202E)) + "Bob"},
		{"LRO", "Alice" + string(rune(0x202D)) + "Bob"},
		{"LRE", "Alice" + string(rune(0x202A)) + "Bob"},
		{"RLE", "Alice" + string(rune(0x202B)) + "Bob"},
		{"PDF", "Alice" + string(rune(0x202C)) + "Bob"},
		{"LRI", "Alice" + string(rune(0x2066)) + "Bob"},
		{"RLI", "Alice" + string(rune(0x2067)) + "Bob"},
		{"FSI", "Alice" + string(rune(0x2068)) + "Bob"},
		{"PDI", "Alice" + string(rune(0x2069)) + "Bob"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateDisplayName(tt.value); err == nil {
				t.Errorf("validateDisplayName with %s = nil, want an error", tt.name)
			}
		})
	}
}

// TestValidateDisplayName_DirectionalMarksAreAllowed confirms the
// narrower-than-you'd-guess scope of the block: LRM/RLM are hints, not
// overrides, and cannot by themselves reorder rendering — see
// bidiControlChars' doc comment for why they're deliberately excluded.
func TestValidateDisplayName_DirectionalMarksAreAllowed(t *testing.T) {
	name := "Alice" + string(rune(0x200E)) + "Bob" // LRM
	if err := validateDisplayName(name); err != nil {
		t.Errorf("validateDisplayName with LRM = %v, want nil (a mark is not an override)", err)
	}
}

func TestValidateDisplayName_TooLong(t *testing.T) {
	name := strings.Repeat("a", maxDisplayNameLength+1)
	if err := validateDisplayName(name); err == nil {
		t.Errorf("validateDisplayName(%d chars) = nil, want an error", maxDisplayNameLength+1)
	}
}

// TestValidateDisplayName_TooLongCountsRunesNotBytes: a multi-byte script
// must not be penalized for its UTF-8 encoding size.
func TestValidateDisplayName_TooLongCountsRunesNotBytes(t *testing.T) {
	name := strings.Repeat("日", maxDisplayNameLength) // 3 bytes/rune, well over maxDisplayNameLength bytes
	if err := validateDisplayName(name); err != nil {
		t.Errorf("validateDisplayName(%d runes of a multi-byte script) = %v, want nil", maxDisplayNameLength, err)
	}
}

// newTestPool follows notifications-svc's convention: DB tests skip
// rather than fail when there's no database to talk to.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set; skipping test that requires a live postgres (see docker-compose.yml)")
	}
	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func randomUUID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		t.Fatalf("generate uuid: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// insertTestUser inserts a minimal, real users row (active, so it behaves
// like an account PATCH /profile would actually be called against) and
// registers its cleanup, including any auth_outbox rows a test's calls
// into updateProfile leave behind.
func insertTestUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	userID := randomUUID(t)
	email := fmt.Sprintf("profile-test-%s@example.com", userID)
	_, err := pool.Exec(ctx,
		"INSERT INTO users (id, email, password_hash, status) VALUES ($1, $2, 'not-a-real-hash', 'active')",
		userID, email,
	)
	if err != nil {
		t.Fatalf("insert test user: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if _, err := pool.Exec(ctx, "DELETE FROM auth_outbox WHERE partition_key = $1", userID); err != nil {
			t.Logf("cleanup: delete outbox rows for user %s: %v", userID, err)
		}
		if _, err := pool.Exec(ctx, "DELETE FROM users WHERE id = $1", userID); err != nil {
			t.Logf("cleanup: delete test user %s: %v", userID, err)
		}
	})
	return userID
}

func TestGetProfile_NotFound(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	_, found, err := getProfile(ctx, pool, randomUUID(t))
	if err != nil {
		t.Fatalf("getProfile: unexpected error: %v", err)
	}
	if found {
		t.Error("getProfile for a nonexistent user returned found=true")
	}
}

func TestGetProfile_DefaultsToNilDisplayName(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	userID := insertTestUser(t, ctx, pool)

	profile, found, err := getProfile(ctx, pool, userID)
	if err != nil {
		t.Fatalf("getProfile: unexpected error: %v", err)
	}
	if !found {
		t.Fatal("getProfile did not find the freshly inserted user")
	}
	if profile.DisplayName != nil {
		t.Errorf("DisplayName = %v, want nil for a user that never set one", *profile.DisplayName)
	}
}

// TestUpdateProfile_SetsAndClearsDisplayName is the DoD's core claim:
// PATCH saves the name, and — per the task's explicit spec — a later
// clear (nil) actually resets it to NULL rather than leaving it as an
// empty string.
func TestUpdateProfile_SetsAndClearsDisplayName(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	userID := insertTestUser(t, ctx, pool)

	name := "Alice"
	profile, found, err := updateProfile(ctx, pool, userID, &name)
	if err != nil {
		t.Fatalf("updateProfile (set): unexpected error: %v", err)
	}
	if !found {
		t.Fatal("updateProfile (set) did not find the user")
	}
	if profile.DisplayName == nil || *profile.DisplayName != "Alice" {
		t.Fatalf("DisplayName after set = %v, want \"Alice\"", profile.DisplayName)
	}

	// Independently re-read, not just trust the RETURNING clause.
	reread, _, err := getProfile(ctx, pool, userID)
	if err != nil {
		t.Fatalf("getProfile after set: %v", err)
	}
	if reread.DisplayName == nil || *reread.DisplayName != "Alice" {
		t.Fatalf("re-read DisplayName = %v, want \"Alice\"", reread.DisplayName)
	}

	profile, found, err = updateProfile(ctx, pool, userID, nil)
	if err != nil {
		t.Fatalf("updateProfile (clear): unexpected error: %v", err)
	}
	if !found {
		t.Fatal("updateProfile (clear) did not find the user")
	}
	if profile.DisplayName != nil {
		t.Errorf("DisplayName after clear = %v, want nil (reset to NULL, not an empty string)", *profile.DisplayName)
	}
}

func TestUpdateProfile_NotFound(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	name := "Nobody"
	_, found, err := updateProfile(ctx, pool, randomUUID(t), &name)
	if err != nil {
		t.Fatalf("updateProfile: unexpected error: %v", err)
	}
	if found {
		t.Error("updateProfile for a nonexistent user returned found=true")
	}
}

// TestUpdateProfile_WritesProfileUpdatedOutboxEvent is the other half of
// the DoD: ProfileUpdated must actually be written (same transaction as
// the column update) with the right payload for the outbox relay to
// eventually deliver to notifications-svc.
func TestUpdateProfile_WritesProfileUpdatedOutboxEvent(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	userID := insertTestUser(t, ctx, pool)

	name := "Bob"
	if _, _, err := updateProfile(ctx, pool, userID, &name); err != nil {
		t.Fatalf("updateProfile: unexpected error: %v", err)
	}

	var eventType, partitionKey string
	var payload []byte
	err := pool.QueryRow(ctx,
		"SELECT event_type, partition_key, payload FROM auth_outbox WHERE partition_key = $1 ORDER BY id DESC LIMIT 1",
		userID,
	).Scan(&eventType, &partitionKey, &payload)
	if err != nil {
		t.Fatalf("query auth_outbox: %v", err)
	}
	if eventType != "ProfileUpdated" {
		t.Errorf("event_type = %q, want %q", eventType, "ProfileUpdated")
	}
	if partitionKey != userID {
		t.Errorf("partition_key = %q, want %q (must key on user_id, same as UserActivated, so ordering with it is guaranteed)", partitionKey, userID)
	}

	var event eventsv1.ProfileUpdated
	if err := proto.Unmarshal(payload, &event); err != nil {
		t.Fatalf("unmarshal ProfileUpdated payload: %v", err)
	}
	if event.GetUserId() != userID {
		t.Errorf("event.UserId = %q, want %q", event.GetUserId(), userID)
	}
	if event.GetDisplayName() != "Bob" {
		t.Errorf("event.DisplayName = %q, want %q", event.GetDisplayName(), "Bob")
	}
	if event.GetEventId() == "" {
		t.Error("event.EventId is empty, want a generated UUID")
	}
}

// TestUpdateProfile_ClearEventHasEmptyDisplayName pins the wire
// convention ProfileUpdated's doc comment states: clearing the name
// publishes an empty string, not any other sentinel.
func TestUpdateProfile_ClearEventHasEmptyDisplayName(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	userID := insertTestUser(t, ctx, pool)

	name := "Carol"
	if _, _, err := updateProfile(ctx, pool, userID, &name); err != nil {
		t.Fatalf("updateProfile (set): unexpected error: %v", err)
	}
	if _, _, err := updateProfile(ctx, pool, userID, nil); err != nil {
		t.Fatalf("updateProfile (clear): unexpected error: %v", err)
	}

	var payload []byte
	err := pool.QueryRow(ctx,
		"SELECT payload FROM auth_outbox WHERE partition_key = $1 ORDER BY id DESC LIMIT 1",
		userID,
	).Scan(&payload)
	if err != nil {
		t.Fatalf("query auth_outbox: %v", err)
	}
	var event eventsv1.ProfileUpdated
	if err := proto.Unmarshal(payload, &event); err != nil {
		t.Fatalf("unmarshal ProfileUpdated payload: %v", err)
	}
	if event.GetDisplayName() != "" {
		t.Errorf("event.DisplayName after clear = %q, want \"\"", event.GetDisplayName())
	}
}
