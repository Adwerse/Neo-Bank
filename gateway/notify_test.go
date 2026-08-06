package main

import (
	"encoding/json"
	"sort"
	"testing"
)

// staticResolver returns a resolveAccountUser backed by a fixed map, for
// tests that don't care which accounts get looked up, only what comes
// back.
func staticResolver(byAccount map[string]string) resolveAccountUser {
	return func(accountID string) (string, bool) {
		userID, ok := byAccount[accountID]
		return userID, ok
	}
}

// spyResolver wraps staticResolver and records every account_id it's
// asked about, so a test can assert an account was NEVER queried — the
// direct way to prove the sprint-7 rule holds structurally, not just that
// its output happens to look right.
func spyResolver(byAccount map[string]string) (resolveAccountUser, *[]string) {
	var calls []string
	resolve := func(accountID string) (string, bool) {
		calls = append(calls, accountID)
		userID, ok := byAccount[accountID]
		return userID, ok
	}
	return resolve, &calls
}

// exactJSONFields marshals msg and returns its top-level key set, so a
// test can assert a signal carries ONLY the fields the DoD allows — no
// amount, no balance — structurally rather than by convention.
func exactJSONFields(t *testing.T, msg any) []string {
	t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal signal message: %v", err)
	}
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(b, &asMap); err != nil {
		t.Fatalf("unmarshal signal message: %v", err)
	}
	keys := make([]string, 0, len(asMap))
	for k := range asMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func msgType(t *testing.T, msg any) string {
	t.Helper()
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal signal message: %v", err)
	}
	var typed struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(b, &typed); err != nil {
		t.Fatalf("unmarshal signal message: %v", err)
	}
	return typed.Type
}

func TestSignalsForTransferEvent_Completed_BothPartiesGetOwnSignalsOnly(t *testing.T) {
	resolve := staticResolver(map[string]string{
		"acct-sender":    "user-A",
		"acct-recipient": "user-B",
	})

	signals := signalsForTransferEvent(eventTypeTransferCompleted, "transfer-1", "acct-sender", "acct-recipient", resolve)

	var forA, forB []wsSignal
	for _, sig := range signals {
		switch sig.userID {
		case "user-A":
			forA = append(forA, sig)
		case "user-B":
			forB = append(forB, sig)
		default:
			t.Fatalf("signal addressed to unexpected user %q — no third party should ever appear", sig.userID)
		}
	}

	if len(forA) != 2 || len(forB) != 2 {
		t.Fatalf("got %d signals for A, %d for B, want 2 and 2", len(forA), len(forB))
	}

	for _, group := range [][]wsSignal{forA, forB} {
		types := map[string]bool{}
		for _, sig := range group {
			types[msgType(t, sig.msg)] = true

			fields := exactJSONFields(t, sig.msg)
			for _, f := range fields {
				if f != "type" && f != "transfer_id" {
					t.Errorf("signal message carries unexpected field %q (fields: %v) — no amounts or balances allowed", f, fields)
				}
			}
		}
		if !types["balance.changed"] || !types["transfer.updated"] {
			t.Errorf("expected both balance.changed and transfer.updated, got %v", types)
		}
	}
}

func testSignalsForTransferEvent_TerminalWithoutMovement(t *testing.T, eventType string) {
	t.Helper()
	resolve, calls := spyResolver(map[string]string{
		"acct-sender":    "user-A",
		"acct-recipient": "user-B",
	})

	signals := signalsForTransferEvent(eventType, "transfer-2", "acct-sender", "acct-recipient", resolve)

	if len(signals) != 1 {
		t.Fatalf("got %d signals, want exactly 1 (sender only)", len(signals))
	}
	if signals[0].userID != "user-A" {
		t.Errorf("signal addressed to %q, want user-A (the sender)", signals[0].userID)
	}
	if got := msgType(t, signals[0].msg); got != "transfer.updated" {
		t.Errorf("signal type = %q, want transfer.updated — no balance.changed, nothing moved", got)
	}

	for _, accountID := range *calls {
		if accountID == "acct-recipient" {
			t.Fatalf("resolve() was called with the recipient's account_id for a %s event — sprint 7 requires the recipient never even be looked up for an unsuccessful transfer, calls: %v", eventType, *calls)
		}
	}
}

func TestSignalsForTransferEvent_Failed_OnlySenderNotified_RecipientNeverResolved(t *testing.T) {
	testSignalsForTransferEvent_TerminalWithoutMovement(t, eventTypeTransferFailed)
}

func TestSignalsForTransferEvent_Rejected_OnlySenderNotified_RecipientNeverResolved(t *testing.T) {
	testSignalsForTransferEvent_TerminalWithoutMovement(t, eventTypeTransferRejected)
}

func TestSignalsForDepositEvent_NotifiesOwnerOnly(t *testing.T) {
	resolve := staticResolver(map[string]string{"acct-owner": "user-A"})

	signals := signalsForDepositEvent("deposit-1", "acct-owner", resolve)

	if len(signals) != 2 {
		t.Fatalf("got %d signals, want 2 (balance.changed + deposit.updated)", len(signals))
	}
	types := map[string]bool{}
	for _, sig := range signals {
		if sig.userID != "user-A" {
			t.Errorf("signal addressed to %q, want user-A", sig.userID)
		}
		types[msgType(t, sig.msg)] = true
	}
	if !types["balance.changed"] || !types["deposit.updated"] {
		t.Errorf("expected both balance.changed and deposit.updated, got %v", types)
	}
}

func TestSignalsForTransferEvent_UnresolvedAccount_ProducesNoSignalForThatSide(t *testing.T) {
	resolve := staticResolver(map[string]string{"acct-sender": "user-A"}) // recipient deliberately absent

	signals := signalsForTransferEvent(eventTypeTransferCompleted, "transfer-3", "acct-sender", "acct-recipient", resolve)

	for _, sig := range signals {
		if sig.userID != "user-A" {
			t.Errorf("got a signal for %q, want only user-A (recipient's account never resolved)", sig.userID)
		}
	}
	if len(signals) != 2 {
		t.Errorf("got %d signals, want 2 (sender's balance.changed + transfer.updated only)", len(signals))
	}
}

func TestSignalsForDepositEvent_UnresolvedAccount_ProducesNoSignal(t *testing.T) {
	resolve := staticResolver(map[string]string{})

	signals := signalsForDepositEvent("deposit-2", "acct-unknown", resolve)

	if signals != nil {
		t.Errorf("got %d signals, want none for an unresolved account", len(signals))
	}
}
