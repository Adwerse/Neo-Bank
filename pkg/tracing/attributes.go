package tracing

import "go.opentelemetry.io/otel/attribute"

// The domain attribute keys this repo puts on spans, defined in one place
// so that what ends up in Jaeger is a reviewable list rather than
// whatever each call site felt like naming things.
//
// # WHAT MUST NEVER GO ON A SPAN
//
// A trace is a data egress channel with exactly the same reach as a log
// line, and the Jaeger running in this compose stack has no
// authentication, no TLS and no retention policy. Anything attached to a
// span is readable by anyone who can open the UI, and it is copied
// verbatim into the collector's storage. So:
//
//   - no credentials of any kind: JWTs, access/refresh tokens, session
//     ids, passwords, password hashes, the JWT signing secret
//   - no email addresses, names, or anything else identifying a person
//   - no card data, and no Stripe client_secret (it authorises confirming
//     a payment — it is a credential, despite living in a response body)
//   - no account NUMBERS, the human-facing identifier a user would read
//     out loud. Internal account UUIDs are fine and are what the keys
//     below carry: they are pseudonymous, meaningless outside this
//     database, and without them a trace cannot be tied to the row it
//     touched.
//
// Note that this is a stricter rule than "don't log secrets", because
// automatic instrumentation attaches things without being asked.
// otelhttp records URLs, so a secret in a query string would land on a
// span even though no line of this repo put it there — which is one more
// reason this codebase keeps credentials in headers and bodies.
//
// # AMOUNTS ARE DELIBERATELY INCLUDED
//
// AttrAmountMinor carries the transfer/deposit amount in minor units.
// That is a real disclosure decision, not an oversight: an amount is
// commercially sensitive, and combined with a timestamp it is a weak
// identifier. It is included anyway because a money-movement trace
// without the amount cannot answer the questions traces exist for — "did
// this transfer get rejected for the amount, or the velocity rule?",
// "did the ledger post what transfers-svc thought it was posting?" —
// and debugging money by correlating back to the database on every
// question defeats the purpose. Recorded as a plain integer with no
// currency and no account number attached, so a leaked span says "1500"
// and not "1500 EUR from account X to account Y". Documented in the
// README so the trade is visible rather than folded into a constant.
const (
	// AttrTransferID is transfers.id — the resource, not the ledger
	// transaction it eventually produces (that is AttrLedgerTransactionID,
	// and the two being different is exactly the sort of thing a trace
	// should make obvious).
	AttrTransferID = "neobank.transfer.id"

	// AttrDepositID is deposits.id.
	AttrDepositID = "neobank.deposit.id"

	// AttrAccountID is an internal account UUID. See the note above on why
	// this is acceptable and an account number is not.
	AttrAccountID = "neobank.account.id"

	// AttrAmountMinor is an amount in minor units (cents). See the note
	// above — this one is a deliberate trade.
	AttrAmountMinor = "neobank.amount_minor"

	// AttrTransferStatus is the transfer's resulting status
	// (completed/failed/rejected/pending).
	AttrTransferStatus = "neobank.transfer.status"

	// AttrTransferOutcome is the internal branch taken, which is finer
	// grained than status: a transfer can end "pending" because fraud-svc
	// was unreachable or because the ledger call's result was unknown, and
	// those are different incidents.
	AttrTransferOutcome = "neobank.transfer.outcome"

	// AttrFraudDecision is fraud-svc's verdict: approve or reject.
	AttrFraudDecision = "neobank.fraud.decision"

	// AttrFraudTriggeredRule is the rule that fired, empty when none did.
	// The attribute that makes "show me everything the velocity rule
	// blocked today" a Jaeger query.
	AttrFraudTriggeredRule = "neobank.fraud.triggered_rule"

	// AttrLedgerTransactionID is the ledger's transaction_id — the id both
	// entries of the double-entry pair share.
	AttrLedgerTransactionID = "neobank.ledger.transaction_id"

	// AttrLedgerOutcome is the ledger's own result for a posting attempt
	// (posted / insufficient_funds / account_not_found).
	AttrLedgerOutcome = "neobank.ledger.outcome"

	// AttrErrorType is the low-cardinality failure class Fail records. Kept
	// as a repo-owned key rather than OTel's semconv error.type because
	// the values here are domain outcomes ("fraud_check_unavailable"),
	// not exception class names.
	AttrErrorType = "neobank.error.type"

	// AttrIdempotencyReplay marks a request that returned an existing
	// resource instead of doing the work — the difference between "this
	// transfer was fast" and "this transfer never happened on this call".
	AttrIdempotencyReplay = "neobank.idempotency.replayed"
)

// Convenience constructors, so call sites read as domain statements and
// the value types stay consistent (an amount is always an int64, never a
// string on one span and an int on another — Jaeger will happily store
// both and then refuse to compare them).

func TransferID(id string) attribute.KeyValue { return attribute.String(AttrTransferID, id) }
func DepositID(id string) attribute.KeyValue  { return attribute.String(AttrDepositID, id) }
func AccountID(id string) attribute.KeyValue  { return attribute.String(AttrAccountID, id) }
func AmountMinor(v int64) attribute.KeyValue  { return attribute.Int64(AttrAmountMinor, v) }

func TransferStatus(s string) attribute.KeyValue {
	return attribute.String(AttrTransferStatus, s)
}

func TransferOutcome(s string) attribute.KeyValue {
	return attribute.String(AttrTransferOutcome, s)
}

func FraudDecision(s string) attribute.KeyValue {
	return attribute.String(AttrFraudDecision, s)
}

func FraudTriggeredRule(s string) attribute.KeyValue {
	return attribute.String(AttrFraudTriggeredRule, s)
}

func LedgerTransactionID(id string) attribute.KeyValue {
	return attribute.String(AttrLedgerTransactionID, id)
}

func LedgerOutcome(s string) attribute.KeyValue {
	return attribute.String(AttrLedgerOutcome, s)
}

func IdempotencyReplay(v bool) attribute.KeyValue {
	return attribute.Bool(AttrIdempotencyReplay, v)
}
