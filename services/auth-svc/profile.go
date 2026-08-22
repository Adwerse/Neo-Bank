package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"neobank/pkg/outbox"
	eventsv1 "neobank/proto/gen/go/events/v1"
)

// maxDisplayNameLength is a character (not byte) count, checked after
// trimming — long enough for a real name in any script, short enough that
// it can't be used to push meaningful content through a field this system
// treats as a short label everywhere it's rendered.
const maxDisplayNameLength = 50

// bidiControlChars are the Unicode bidirectional override/isolate control
// characters: LRE, RLE, PDF, LRO, RLO (explicit overrides) and LRI, RLI,
// FSI, PDI (isolates), U+202A through U+202E and U+2066 through U+2069.
// Blocked because they let a display name render differently from its
// underlying byte sequence — the classic case is RLO making a name read
// right-to-left so it visually spoofs a different string than what's
// actually stored, the same trick used in RLO filename-spoofing attacks.
// Directional MARKS (LRM U+200E, RLM U+200F) are deliberately not in this
// list: they're hints, not overrides, and cannot by themselves reorder
// how surrounding text renders.
//
// Built from bare hex code points, not rune literals containing the
// actual characters: these ARE bidi override/isolate control codes, so
// pasting them verbatim into this file would make the source itself
// subject to the exact rendering trick this code exists to reject — an
// editor or terminal honoring them could display this file's own
// contents out of byte order.
var bidiControlChars = map[rune]struct{}{
	rune(0x202A): {}, // LRE — Left-to-Right Embedding
	rune(0x202B): {}, // RLE — Right-to-Left Embedding
	rune(0x202C): {}, // PDF — Pop Directional Formatting
	rune(0x202D): {}, // LRO — Left-to-Right Override
	rune(0x202E): {}, // RLO — Right-to-Left Override (the classic spoofing character)
	rune(0x2066): {}, // LRI — Left-to-Right Isolate
	rune(0x2067): {}, // RLI — Right-to-Left Isolate
	rune(0x2068): {}, // FSI — First Strong Isolate
	rune(0x2069): {}, // PDI — Pop Directional Isolate
}

// validateDisplayName checks an already-trimmed, non-empty candidate.
// Trimming and the empty-means-reset-to-NULL decision both happen in the
// caller (updateProfileHandler) — this function only judges the content
// that would actually be stored.
func validateDisplayName(s string) error {
	if utf8.RuneCountInString(s) > maxDisplayNameLength {
		return fmt.Errorf("display_name must be %d characters or fewer", maxDisplayNameLength)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("display_name must not contain control characters")
		}
		if _, ok := bidiControlChars[r]; ok {
			return errors.New("display_name must not contain text-direction control characters")
		}
	}
	return nil
}

// Profile is the JSON representation of a user's profile — the response
// shape for GET /profile, PATCH /profile, and POST /profile/avatar/confirm.
// AvatarKey is the raw storage key (harmless to expose: it's meaningless
// without this service's credentials to sign a request for it, see
// storage.go). AvatarURL64/AvatarURL256 are presigned GET URLs, freshly
// signed on every response that includes them (see withAvatarURLs,
// avatar.go) — omitted entirely, not merely null, when there's no avatar.
type Profile struct {
	UserID          string     `json:"user_id"`
	DisplayName     *string    `json:"display_name"`
	AvatarKey       *string    `json:"avatar_key"`
	AvatarUpdatedAt *time.Time `json:"avatar_updated_at"`
	AvatarURL64     *string    `json:"avatar_url_64,omitempty"`
	AvatarURL256    *string    `json:"avatar_url_256,omitempty"`
}

func getProfileHandler(pool *pgxpool.Pool, storage *avatarStorage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		userID := r.Header.Get("X-User-Id")
		if userID == "" {
			writeJSONError(w, http.StatusBadRequest, "missing X-User-Id header")
			return
		}

		profile, found, err := getProfile(r.Context(), pool, userID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		if !found {
			writeJSONError(w, http.StatusNotFound, "user not found")
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(withAvatarURLs(r.Context(), storage, profile))
	}
}

// updateProfileRequest carries the one field this endpoint accepts today.
// An empty string (explicit "" or the field simply omitted — both decode
// to the same Go zero value) means "clear the display name", per the
// task's own spec: there is nothing else in this narrow request body for
// omission to mean "leave unchanged" relative to.
type updateProfileRequest struct {
	DisplayName string `json:"display_name"`
}

func updateProfileHandler(pool *pgxpool.Pool, storage *avatarStorage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		userID := r.Header.Get("X-User-Id")
		if userID == "" {
			writeJSONError(w, http.StatusBadRequest, "missing X-User-Id header")
			return
		}

		var req updateProfileRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid request body")
			return
		}

		// Trim first, then judge the result: leading/trailing whitespace is
		// never itself a validation failure, and a name that's ALL
		// whitespace trims to "" — a reset, not a "your name is blank" error.
		trimmed := strings.TrimSpace(req.DisplayName)
		var displayName *string
		if trimmed != "" {
			if err := validateDisplayName(trimmed); err != nil {
				writeJSONError(w, http.StatusBadRequest, err.Error())
				return
			}
			displayName = &trimmed
		}

		profile, found, err := updateProfile(r.Context(), pool, userID, displayName)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to process request")
			return
		}
		if !found {
			writeJSONError(w, http.StatusNotFound, "user not found")
			return
		}

		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(withAvatarURLs(r.Context(), storage, profile))
	}
}

func getProfile(ctx context.Context, pool *pgxpool.Pool, userID string) (Profile, bool, error) {
	var p Profile
	err := pool.QueryRow(ctx,
		"SELECT id, display_name, avatar_key, avatar_updated_at FROM users WHERE id = $1",
		userID,
	).Scan(&p.UserID, &p.DisplayName, &p.AvatarKey, &p.AvatarUpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, false, nil
	}
	if err != nil {
		return Profile{}, false, err
	}
	return p, true, nil
}

// updateProfile writes the new display_name and the ProfileUpdated outbox
// event in one transaction — either both land or neither does, the same
// dual-write-gap fix verifyEmailCode already applies to UserActivated.
// displayName is nil to clear the column to SQL NULL (pgx binds a nil
// *string as NULL), or a pointer to an already-validated, already-trimmed
// value to set it.
func updateProfile(ctx context.Context, pool *pgxpool.Pool, userID string, displayName *string) (Profile, bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Profile{}, false, err
	}
	defer tx.Rollback(ctx)

	var p Profile
	err = tx.QueryRow(ctx,
		`UPDATE users SET display_name = $1, updated_at = now() WHERE id = $2
		 RETURNING id, display_name, avatar_key, avatar_updated_at`,
		displayName, userID,
	).Scan(&p.UserID, &p.DisplayName, &p.AvatarKey, &p.AvatarUpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, false, nil
	}
	if err != nil {
		return Profile{}, false, err
	}

	eventID, err := outbox.GenerateEventID()
	if err != nil {
		return Profile{}, false, err
	}
	// eventDisplayName mirrors the DB value's NULL-vs-value distinction on
	// the wire as ""-vs-value — see ProfileUpdated's doc comment in
	// user_events.proto for why that's unambiguous here specifically (a
	// stored display_name is never itself "", only ever NULL or a
	// non-empty trimmed string).
	var eventDisplayName string
	if displayName != nil {
		eventDisplayName = *displayName
	}
	payload, err := proto.Marshal(&eventsv1.ProfileUpdated{
		UserId:      userID,
		DisplayName: eventDisplayName,
		OccurredAt:  timestamppb.New(time.Now()),
		EventId:     eventID,
	})
	if err != nil {
		return Profile{}, false, err
	}
	if err := outbox.InsertEvent(ctx, tx, authOutboxTable, eventID, "ProfileUpdated", userID, payload); err != nil {
		return Profile{}, false, err
	}

	return p, true, tx.Commit(ctx)
}
