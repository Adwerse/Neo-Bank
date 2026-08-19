package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Fixtures is the contract between `lt setup` (Go, writes it) and the k6
// scripts (JavaScript, read it at init time). It is a plain file rather
// than an env var or a k6 remote-data source because k6's init context can
// open() a local file with no network round trip, and because a
// human-readable fixtures.json is the fastest way to see what a run was
// actually pointed at when a result looks wrong.
//
// Access tokens live here. They are 15-minute JWTs for throwaway
// @loadtest.local users on a local dev stack, which is why writing them to
// disk is acceptable — loadtest/fixtures/ is gitignored regardless.
type Fixtures struct {
	// RunPrefix namespaces everything a run creates: user emails, and (via
	// the k6 scripts) idempotency keys. Verification scopes its queries by
	// it, so a stack that has other data on it — and a dev stack always
	// does — does not pollute the cohort being checked.
	RunPrefix string `json:"run_prefix"`
	// GatewayURL as seen from wherever the load generator runs. `lt setup`
	// itself uses the host-published port; the k6 container is given the
	// in-network address instead, since it runs on neo-bank_default.
	GatewayURL string    `json:"gateway_url"`
	CreatedAt  time.Time `json:"created_at"`
	// FundedPerAccount is what each account was topped up to at setup. It
	// is the ceiling on how much any one account can send before
	// insufficient_funds starts shaping the results, so it is recorded
	// rather than left implicit.
	FundedPerAccount int64         `json:"funded_per_account"`
	Users            []FixtureUser `json:"users"`
}

type FixtureUser struct {
	Email       string `json:"email"`
	UserID      string `json:"user_id"`
	AccountID   string `json:"account_id"`
	IBAN        string `json:"iban"`
	AccessToken string `json:"access_token"`
}

// AccountIDs is what the verifier needs: the cohort, as ledger-svc knows
// it. accounts.id and ledger_accounts.account_id are the same value by
// construction (see accounts-svc's CreateLedgerAccount call), so one list
// serves both sides of every invariant query.
func (f Fixtures) AccountIDs() []string {
	ids := make([]string, 0, len(f.Users))
	for _, u := range f.Users {
		ids = append(ids, u.AccountID)
	}
	return ids
}

func writeFixtures(path string, f Fixtures) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create fixtures directory: %w", err)
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal fixtures: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write fixtures: %w", err)
	}
	return nil
}

func readFixtures(path string) (Fixtures, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Fixtures{}, fmt.Errorf("read fixtures (run `lt setup` first): %w", err)
	}
	var f Fixtures
	if err := json.Unmarshal(data, &f); err != nil {
		return Fixtures{}, fmt.Errorf("parse fixtures: %w", err)
	}
	if len(f.Users) == 0 {
		return Fixtures{}, fmt.Errorf("fixtures %s contains no users", path)
	}
	return f, nil
}
