package main

import (
	"fmt"
	"log"
	"net/smtp"
	"strings"
	"time"
)

// sendAttempts/sendRetryDelay bound the in-process retry around an SMTP
// send. This retry — not the "don't commit the offset on error" pattern
// the consumer loops share — is what actually makes a transient mail
// outage survivable: kafka-go's Reader never redelivers a message inside
// a running process (see runTransferEventsConsumer), so a handler that
// gives up on the first refused connection has, in practice, dropped the
// notification.
const sendAttempts = 3
const sendRetryDelay = 500 * time.Millisecond

// emailFooter closes every message. Split out so the ASCII assertions in
// email_test.go cover it once rather than four times.
const emailFooter = "This is an automated message from Neo-Bank. Please do not reply."

// dateLayout renders occurred_at. UTC and explicit about it: this system
// stores no user timezone, and an unlabelled local-looking timestamp in a
// bank's email is worse than an obviously-UTC one.
const dateLayout = "2006-01-02 15:04:05 UTC"

// email is a rendered message, ready to hand to sendEmail. Subject and
// Body are kept apart rather than pre-joined into a blob so tests can
// assert on the body alone — several of this sprint's requirements are
// statements about what the body must NOT contain.
type email struct {
	Subject string
	Body    string
}

// failureReasonSentences maps transfers-svc's failure_reason vocabulary
// (services/transfers-svc/transfer.go) to something a customer can act
// on. Deliberately has NO fallback to the raw token, mirroring the
// frontend's REJECTED_REASON_LABELS: "ledger_internal_error" is an
// internal identifier, and an unmapped code drops the Reason line
// entirely rather than leaking one.
var failureReasonSentences = map[string]string{
	"insufficient_funds":    "There were not enough funds in your account.",
	"account_not_found":     "The recipient account could not be found.",
	"invalid_amount":        "The transfer amount was not valid.",
	"ledger_internal_error": "A technical problem prevented the transfer.",
	"timeout_unresolved":    "The transfer could not be confirmed in time.",
}

// buildTransferSentEmail is the sender's half of a completed transfer.
func buildTransferSentEmail(amount int64, transferID, toAccountNumber string, at time.Time) email {
	var b strings.Builder
	fmt.Fprintf(&b, "Your transfer of %s has been sent.\r\n\r\n", formatAmount(amount))
	fmt.Fprintf(&b, "Transfer ID: %s\r\n", transferID)
	writeAccountLine(&b, "To account", toAccountNumber)
	writeDateLine(&b, at)
	fmt.Fprintf(&b, "\r\n%s\r\n", emailFooter)
	return email{Subject: "Neo-Bank: transfer sent", Body: b.String()}
}

// buildTransferReceivedEmail is the recipient's half of a completed
// transfer, and the only message in this service that says anything at
// all about a third party.
//
// Note what it is not given: the sender's email address, and any balance
// on either side. The recipient needs to know money arrived, how much,
// and which account it came from — the parameter list is the enforcement,
// so there is nothing here to leak by accident.
func buildTransferReceivedEmail(amount int64, transferID, fromAccountNumber string, at time.Time) email {
	var b strings.Builder
	fmt.Fprintf(&b, "You have received a transfer of %s.\r\n\r\n", formatAmount(amount))
	writeAccountLine(&b, "From account", fromAccountNumber)
	fmt.Fprintf(&b, "Transfer ID: %s\r\n", transferID)
	writeDateLine(&b, at)
	fmt.Fprintf(&b, "\r\n%s\r\n", emailFooter)
	return email{Subject: "Neo-Bank: transfer received", Body: b.String()}
}

// buildTransferFailedEmail goes to the sender only — TransferFailed means
// the ledger declined and no money moved, so there is no recipient with
// anything to be told about.
func buildTransferFailedEmail(amount int64, transferID, reason string, at time.Time) email {
	var b strings.Builder
	fmt.Fprintf(&b, "Your transfer of %s could not be completed.\r\n", formatAmount(amount))
	b.WriteString("No money has been taken from your account.\r\n\r\n")
	if sentence, ok := failureReasonSentences[reason]; ok {
		fmt.Fprintf(&b, "Reason: %s\r\n", sentence)
	}
	fmt.Fprintf(&b, "Transfer ID: %s\r\n", transferID)
	writeDateLine(&b, at)
	fmt.Fprintf(&b, "\r\n%s\r\n", emailFooter)
	return email{Subject: "Neo-Bank: transfer failed", Body: b.String()}
}

// buildTransferDeclinedEmail is the fraud-rejection message, sender only.
//
// It takes no rule name and no threshold, and that is the point: the
// TransferRejected event carries triggered_rule, but naming the rule that
// tripped ("velocity_count") or the limit it enforces ("over 5,000.00 in
// one transfer") turns a security notice into instructions for staying
// just under the line. The frontend reached the same conclusion in
// sprint 6 (see TransferForm.tsx's REJECTED_REASON_LABELS, which maps
// rule names to vague phrases with no raw-string fallback); an email is
// if anything a worse place to say it, since it is forwardable and
// permanent. Making it a parameter this function does not accept means
// no future edit can leak it without first noticing the doc comment.
func buildTransferDeclinedEmail(amount int64, transferID string, at time.Time) email {
	var b strings.Builder
	fmt.Fprintf(&b, "Your transfer of %s was declined by our automated security checks.\r\n", formatAmount(amount))
	b.WriteString("No money has been taken from your account.\r\n\r\n")
	fmt.Fprintf(&b, "Transfer ID: %s\r\n", transferID)
	writeDateLine(&b, at)
	b.WriteString("\r\nIf you believe this is a mistake, please contact Neo-Bank support.\r\n")
	fmt.Fprintf(&b, "\r\n%s\r\n", emailFooter)
	return email{Subject: "Neo-Bank: transfer declined", Body: b.String()}
}

// formatAmount is the one place " EUR" is appended, so the currency can't
// drift between templates. See money.go for why the symbol isn't used.
func formatAmount(minorUnits int64) string {
	return formatMinorUnits(minorUnits) + " EUR"
}

// writeAccountLine omits the line entirely when the number is unknown
// (""), rather than printing an empty or placeholder value. "" happens
// when the counterparty has no user_contacts row yet, or has one from
// before migration 000003 added account_number — a real possibility, not
// a corner case, and "You have received a transfer of 25.00 EUR" without
// a From account line is a perfectly good email.
func writeAccountLine(b *strings.Builder, label, accountNumber string) {
	if accountNumber == "" {
		return
	}
	fmt.Fprintf(b, "%s: %s\r\n", label, accountNumber)
}

// writeDateLine omits the line for a zero time — eventTime returns one
// for a missing or invalid occurred_at, and no date beats "1970-01-01".
func writeDateLine(b *strings.Builder, at time.Time) {
	if at.IsZero() {
		return
	}
	fmt.Fprintf(b, "Date: %s\r\n", at.Format(dateLayout))
}

// sendEmail follows auth-svc's sendVerificationEmail exactly: stdlib
// net/smtp, a nil Auth (Mailpit wants no credentials), and headers built
// with fmt.Sprintf rather than a templating library this repo doesn't
// use. Moving to a real provider is a change of addr/Auth here, not a
// rewrite of anything above.
//
// No MIME-Version or Content-Type header, matching auth-svc: the bodies
// are plain ASCII text, which is what a mail client assumes by default.
// email_test.go asserts that premise so it can't quietly stop being true.
func sendEmail(addr, from, to string, m email) error {
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s", from, to, m.Subject, m.Body)
	return smtp.SendMail(addr, nil, from, []string{to}, []byte(msg))
}

// sendEmailWithRetry retries a failed send a few times before giving up.
// See sendAttempts for why this, and not the offset-commit pattern, is
// the durability mechanism that matters here.
func sendEmailWithRetry(addr, from, to string, m email) error {
	var err error
	for attempt := 1; attempt <= sendAttempts; attempt++ {
		if err = sendEmail(addr, from, to, m); err == nil {
			return nil
		}
		log.Printf("notifications-svc: smtp send to %s failed (attempt %d/%d): %v", to, attempt, sendAttempts, err)
		if attempt < sendAttempts {
			time.Sleep(sendRetryDelay)
		}
	}
	return fmt.Errorf("send email to %s after %d attempts: %w", to, sendAttempts, err)
}
