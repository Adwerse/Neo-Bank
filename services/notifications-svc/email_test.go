package main

import (
	"strings"
	"testing"
	"time"
)

var testTime = time.Date(2026, 8, 2, 14, 3, 11, 0, time.UTC)

const testTransferID = "4f2c9e11-8a3d-4b7e-9c10-2d5a6f8b3c41"

func allTestEmails() map[string]email {
	return map[string]email{
		"sent":             buildTransferSentEmail(123456, testTransferID, "NB0012345678", testTime),
		"received":         buildTransferReceivedEmail(123456, testTransferID, "NB0098765432", testTime),
		"failed":           buildTransferFailedEmail(123456, testTransferID, "insufficient_funds", testTime),
		"declined":         buildTransferDeclinedEmail(123456, testTransferID, testTime),
		"deposit_credited": buildDepositCreditedEmail(123456, testTransferID, testTime),
	}
}

// TestDeclinedEmail_LeaksNoFraudRule is this sprint's central privacy
// requirement made executable: a declined-transfer email must not name
// the rule that tripped or the threshold it enforces, because exact
// limits in a customer's inbox are instructions for staying under them.
func TestDeclinedEmail_LeaksNoFraudRule(t *testing.T) {
	m := buildTransferDeclinedEmail(600000, testTransferID, testTime)
	full := m.Subject + "\n" + m.Body

	for _, rule := range []string{"amount_threshold", "velocity_count", "velocity_sum"} {
		if strings.Contains(full, rule) {
			t.Errorf("declined email contains fraud rule name %q:\n%s", rule, full)
		}
	}
	// The seeded thresholds themselves (fraud-svc migration 000003) and
	// the vocabulary that would introduce one.
	for _, leak := range []string{"500000", "1000000", "threshold", "limit", "velocity", "rule"} {
		if strings.Contains(strings.ToLower(full), leak) {
			t.Errorf("declined email contains %q, which hints at the rule or its threshold:\n%s", leak, full)
		}
	}
}

// TestReceivedEmail_TellsRecipientOnlyWhatTheyNeed — the recipient learns
// the amount and which account it came from, and nothing else about the
// sender.
func TestReceivedEmail_TellsRecipientOnlyWhatTheyNeed(t *testing.T) {
	m := buildTransferReceivedEmail(2550, testTransferID, "NB0098765432", testTime)

	if !strings.Contains(m.Body, "NB0098765432") {
		t.Errorf("received email omits the sender's account number:\n%s", m.Body)
	}
	if !strings.Contains(m.Body, "25.50 EUR") {
		t.Errorf("received email omits the amount:\n%s", m.Body)
	}
	for _, leak := range []string{"@", "balance", "Balance"} {
		if strings.Contains(m.Body, leak) {
			t.Errorf("received email contains %q — it must carry neither the sender's email address nor any balance:\n%s", leak, m.Body)
		}
	}
}

// TestAccountLine_OmittedWhenUnknown covers the degrade path for a
// counterparty with no user_contacts row, or one linked before migration
// 000003 added account_number. The line disappears; nothing renders empty.
func TestAccountLine_OmittedWhenUnknown(t *testing.T) {
	received := buildTransferReceivedEmail(2550, testTransferID, "", testTime)
	if strings.Contains(received.Body, "From account") {
		t.Errorf("received email kept the From account line with an unknown number:\n%s", received.Body)
	}
	if !strings.Contains(received.Body, "25.50 EUR") {
		t.Errorf("received email lost the amount when the account number was unknown:\n%s", received.Body)
	}

	sent := buildTransferSentEmail(2550, testTransferID, "", testTime)
	if strings.Contains(sent.Body, "To account") {
		t.Errorf("sent email kept the To account line with an unknown number:\n%s", sent.Body)
	}
}

// TestFailedEmail_ReasonMapping — every known code renders as a sentence,
// and an unknown one drops the line rather than printing the raw token.
func TestFailedEmail_ReasonMapping(t *testing.T) {
	for reason, sentence := range failureReasonSentences {
		m := buildTransferFailedEmail(123456, testTransferID, reason, testTime)
		if !strings.Contains(m.Body, sentence) {
			t.Errorf("failed email for reason %q omits its sentence %q:\n%s", reason, sentence, m.Body)
		}
		if strings.Contains(m.Body, reason) {
			t.Errorf("failed email for reason %q contains the raw code:\n%s", reason, m.Body)
		}
	}

	unknown := buildTransferFailedEmail(123456, testTransferID, "some_new_ledger_code", testTime)
	if strings.Contains(unknown.Body, "Reason:") {
		t.Errorf("failed email kept a Reason line for an unmapped code:\n%s", unknown.Body)
	}
	if strings.Contains(unknown.Body, "some_new_ledger_code") {
		t.Errorf("failed email leaked an unmapped raw code:\n%s", unknown.Body)
	}
	if !strings.Contains(unknown.Body, "No money has been taken") {
		t.Errorf("failed email lost its reassurance line for an unmapped code:\n%s", unknown.Body)
	}
}

// TestEmails_AreASCII is the premise sendEmail's missing charset header
// rests on.
func TestEmails_AreASCII(t *testing.T) {
	for name, m := range allTestEmails() {
		if !isASCII(m.Subject) {
			t.Errorf("%s subject is not ASCII: %q", name, m.Subject)
		}
		if !isASCII(m.Body) {
			t.Errorf("%s body is not ASCII: %q", name, m.Body)
		}
	}
}

func TestEmails_AllStateTheAmount(t *testing.T) {
	for name, m := range allTestEmails() {
		if !strings.Contains(m.Body, "1,234.56 EUR") {
			t.Errorf("%s body does not state the amount as %q:\n%s", name, "1,234.56 EUR", m.Body)
		}
		if !strings.Contains(m.Body, testTransferID) {
			t.Errorf("%s body does not state the transfer id:\n%s", name, m.Body)
		}
		if !strings.HasPrefix(m.Subject, "Neo-Bank: ") {
			t.Errorf("%s subject %q does not carry the Neo-Bank prefix", name, m.Subject)
		}
	}
}

// TestDateLine_OmittedForZeroTime — eventTime returns a zero time for a
// missing or invalid occurred_at, and a 1970 date in a customer's email
// is worse than no date at all.
func TestDateLine_OmittedForZeroTime(t *testing.T) {
	m := buildTransferSentEmail(123456, testTransferID, "NB0012345678", time.Time{})
	if strings.Contains(m.Body, "Date:") {
		t.Errorf("email kept a Date line for a zero time:\n%s", m.Body)
	}
	if strings.Contains(m.Body, "1970") {
		t.Errorf("email rendered the unix epoch as a date:\n%s", m.Body)
	}
}
