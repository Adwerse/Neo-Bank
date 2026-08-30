package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"neobank/pkg/outbox"
	"neobank/pkg/tracing"
	accountsv1 "neobank/proto/gen/go/accounts/v1"
	eventsv1 "neobank/proto/gen/go/events/v1"
	fraudv1 "neobank/proto/gen/go/fraud/v1"
	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

// outboxTable is transfers-svc's outbox table name, shared with the outbox
// relay/cleanup workers wired up in main.go — kept as one constant so the
// name is never duplicated as a magic string across markTransferX and the
// workers.
const outboxTable = "outbox"

const (
	accountStatusActive = "active"
	accountStatusClosed = "closed"
)

const uniqueViolation = "23505"

// invalidTextRepresentation is Postgres's SQLSTATE for a malformed typed
// literal (e.g. a non-UUID string bound to a uuid column) — same code
// accounts-svc's isNotFoundErr treats specially. Here it means a
// structurally-valid-looking cursor decoded to a garbage id.
const invalidTextRepresentation = "22P02"

// transfersIdempotencyKeyConstraint is Postgres's default name for a
// single-column UNIQUE constraint (<table>_<column>_key), per the
// transfers migration — not explicitly named there, same convention as
// accounts-svc's accountsAccountNumberConstraint.
const transfersIdempotencyKeyConstraint = "transfers_idempotency_key_key"

const ledgerCallTimeout = 5 * time.Second
const fraudCallTimeout = 5 * time.Second

const (
	failureReasonInsufficientFunds = "insufficient_funds"
	failureReasonAccountNotFound   = "account_not_found"
	failureReasonInvalidAmount     = "invalid_amount"
	failureReasonLedgerInternal    = "ledger_internal_error"
	// failureReasonTimeoutUnresolved is set by the reconciliation worker
	// (reconcile.go), never by settleTransfer itself: it means a transfer
	// sat pending long enough to go stale, and ledger-svc's own data
	// confirmed it never actually executed — not a guess, a fact checked
	// against the source of truth.
	failureReasonTimeoutUnresolved = "timeout_unresolved"
)

// Transfer is a transfers row: transfers-svc's own record of a transfer
// request's lifecycle (pending -> completed/failed), kept separate from
// ledger-svc's entries log because there's a gap between "client asked to
// transfer" and "ledger recorded the entry" where transfers-svc could crash
// or the network could drop the response — the idempotency_key attaches to
// this record, not to a ledger entry.
type Transfer struct {
	ID                 string `json:"id"`
	IdempotencyKey     string `json:"idempotency_key"`
	SenderAccountID    string `json:"sender_account_id"`
	RecipientAccountID string `json:"recipient_account_id"`
	Amount             int64  `json:"amount"`
	Status             string `json:"status"`
	// FailureReason holds one of two disjoint vocabularies depending on
	// Status: a ledger failure code (e.g. "insufficient_funds") when
	// "failed", or fraud-svc's triggered_rule (e.g. "amount_threshold")
	// when "rejected" — the two statuses are mutually exclusive, so which
	// vocabulary applies is never ambiguous.
	FailureReason       *string   `json:"failure_reason,omitempty"`
	LedgerTransactionID *string   `json:"ledger_transaction_id,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type createTransferOutcome int

const (
	createTransferOK createTransferOutcome = iota
	createTransferInvalidAmount
	createTransferInvalidRecipientIban // recipient IBAN failed format/check-digit validation — rejected before any accounts-svc database lookup
	createTransferUnsupportedBank      // recipient IBAN is structurally valid but for a bank code that isn't this one — no SEPA/inter-bank rail exists
	createTransferRecipientNotFound
	createTransferSelfTransfer
	createTransferRecipientClosed
	createTransferSenderNotActive
	createTransferResolveRateLimited // caller has made too many recipient-resolve attempts recently — see accounts-svc's ResolveAccountByIban
	createTransferReplayed           // an existing transfer was found for this idempotency key — do not re-settle, just return its current state
	createTransferKeyReused          // idempotency key reused with different parameters — client bug
)

// String renders the outcome for span attributes (and anything else that
// wants it readable). Explicit strings rather than the iota's number: a
// trace attribute reading "recipient_not_found" is self-describing, while
// "2" requires the reader to have this file open — and the numbers shift
// the moment a constant is inserted anywhere but the end, silently
// re-labelling historical traces.
func (o createTransferOutcome) String() string {
	switch o {
	case createTransferOK:
		return "ok"
	case createTransferInvalidAmount:
		return "invalid_amount"
	case createTransferInvalidRecipientIban:
		return "invalid_recipient_iban"
	case createTransferUnsupportedBank:
		return "unsupported_bank"
	case createTransferRecipientNotFound:
		return "recipient_not_found"
	case createTransferSelfTransfer:
		return "self_transfer"
	case createTransferRecipientClosed:
		return "recipient_closed"
	case createTransferSenderNotActive:
		return "sender_not_active"
	case createTransferResolveRateLimited:
		return "resolve_rate_limited"
	case createTransferReplayed:
		return "replayed"
	case createTransferKeyReused:
		return "idempotency_key_reused"
	default:
		return "unknown"
	}
}

// createTransfer validates a transfer request and, on success, inserts a
// pending transfers row. It never moves any money: ledger-svc's atomic
// ExecuteTransfer (called separately, after this returns) is the only place
// a balance is actually checked or changed — checking it here would race
// that later debit, since the balance could change between this check and
// the debit.
//
// idempotencyKey is caller-supplied and is the single source of truth for
// "have I seen this exact request before" — see getTransferByIdempotencyKey
// and the INSERT's unique-violation handling below for how a retried or
// concurrently-duplicated request is detected and reconciled rather than
// creating (or racing to create) a second row.
//
// userID is the caller's own authenticated identity (the gateway's
// X-User-Id, distinct from senderAccountID which is that user's resolved
// account id) — passed through to ResolveAccountByIban purely so
// accounts-svc can key its per-user resolve rate limit on who is actually
// asking, not on anything client-controlled.
func createTransfer(ctx context.Context, pool *pgxpool.Pool, accountsClient accountsv1.AccountsServiceClient, idempotencyKey, senderAccountID, userID, recipientIban string, amount int64) (Transfer, createTransferOutcome, error) {
	// Fast path: a prior attempt with this exact key already ran (the
	// common case — client retried after a dropped response). Skip
	// re-validating and re-calling accounts-svc; just reconcile and return
	// its current state. This is an optimization, not the correctness
	// guarantee — see the INSERT's unique-violation handling below for
	// that: two concurrent requests could both pass this check before
	// either has inserted, so it alone cannot prevent a duplicate.
	existing, found, err := getTransferByIdempotencyKey(ctx, pool, idempotencyKey)
	if err != nil {
		return Transfer{}, 0, err
	}
	if found {
		return reconcileReplay(ctx, accountsClient, existing, senderAccountID, userID, recipientIban, amount)
	}

	if amount <= 0 {
		return Transfer{}, createTransferInvalidAmount, nil
	}

	recipient, err := accountsClient.ResolveAccountByIban(ctx, &accountsv1.ResolveAccountByIbanRequest{Iban: recipientIban, UserId: userID})
	if err != nil {
		switch status.Code(err) {
		case codes.InvalidArgument:
			return Transfer{}, createTransferInvalidRecipientIban, nil
		case codes.FailedPrecondition:
			return Transfer{}, createTransferUnsupportedBank, nil
		case codes.NotFound:
			return Transfer{}, createTransferRecipientNotFound, nil
		case codes.ResourceExhausted:
			return Transfer{}, createTransferResolveRateLimited, nil
		}
		return Transfer{}, 0, fmt.Errorf("resolve recipient account: %w", err)
	}
	if recipient.GetAccountId() == senderAccountID {
		return Transfer{}, createTransferSelfTransfer, nil
	}
	if recipient.GetStatus() == accountStatusClosed {
		return Transfer{}, createTransferRecipientClosed, nil
	}

	sender, err := accountsClient.GetAccountByID(ctx, &accountsv1.GetAccountByIDRequest{AccountId: senderAccountID})
	if err != nil {
		return Transfer{}, 0, fmt.Errorf("look up sender account: %w", err)
	}
	if sender.GetStatus() != accountStatusActive {
		return Transfer{}, createTransferSenderNotActive, nil
	}

	var t Transfer
	err = pool.QueryRow(ctx,
		`INSERT INTO transfers (idempotency_key, sender_account_id, recipient_account_id, amount, trace_context)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, idempotency_key, sender_account_id, recipient_account_id, amount, status, failure_reason, ledger_transaction_id, created_at, updated_at`,
		idempotencyKey, senderAccountID, recipient.GetAccountId(), amount, nullableTraceContext(ctx),
	).Scan(&t.ID, &t.IdempotencyKey, &t.SenderAccountID, &t.RecipientAccountID, &t.Amount, &t.Status, &t.FailureReason, &t.LedgerTransactionID, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == transfersIdempotencyKeyConstraint {
			// Lost the race: another request with the same key committed
			// first. Postgres's constraint is the actual arbiter here, not
			// the getTransferByIdempotencyKey check above.
			winner, found, ferr := getTransferByIdempotencyKey(ctx, pool, idempotencyKey)
			if ferr != nil {
				return Transfer{}, 0, ferr
			}
			if !found {
				return Transfer{}, 0, fmt.Errorf("idempotency key %s: unique violation but no row found", idempotencyKey)
			}
			return reconcileReplay(ctx, accountsClient, winner, senderAccountID, userID, recipientIban, amount)
		}
		return Transfer{}, 0, fmt.Errorf("insert pending transfer: %w", err)
	}
	return t, createTransferOK, nil
}

func getTransferByIdempotencyKey(ctx context.Context, pool *pgxpool.Pool, idempotencyKey string) (Transfer, bool, error) {
	var t Transfer
	err := pool.QueryRow(ctx,
		"SELECT id, idempotency_key, sender_account_id, recipient_account_id, amount, status, failure_reason, ledger_transaction_id, created_at, updated_at FROM transfers WHERE idempotency_key = $1",
		idempotencyKey,
	).Scan(&t.ID, &t.IdempotencyKey, &t.SenderAccountID, &t.RecipientAccountID, &t.Amount, &t.Status, &t.FailureReason, &t.LedgerTransactionID, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Transfer{}, false, nil
	}
	if err != nil {
		return Transfer{}, false, err
	}
	return t, true, nil
}

const transferHistoryColumns = `id, idempotency_key, sender_account_id, recipient_account_id, amount, status, failure_reason, ledger_transaction_id, created_at, updated_at`

// transferHistoryQueryFirstPage and transferHistoryQueryNextPage are a
// UNION ALL of two independently-indexed branches rather than a single
// WHERE (sender_account_id = $1 OR (recipient_account_id = $1 AND status =
// 'completed')) — deliberately: each branch applies its own LIMIT
// immediately after an index-ordered scan of idx_transfers_sender_created_id
// / idx_transfers_recipient_completed_created_id (see migration 000004),
// which bounds the query's total work at 2*(limit+1) rows no matter how
// large the account's full transfer history is. A single OR'd WHERE across
// two different indexed columns would force Postgres into a BitmapOr +
// Bitmap Heap Scan, which cannot apply LIMIT before materializing (and
// sorting) every matching row — exactly the cost keyset pagination exists
// to avoid. The outer ORDER BY + LIMIT re-merges the two (already small,
// already sorted) branch results; the top-K of a two-way sorted merge is
// always contained within the top-K of each input, so this is exact, not
// an approximation. See ledger-svc's getHistory for the same
// created_at DESC, id DESC tie-break reasoning (created_at can collide
// within one transaction-local now()).
//
// The recipient branch only ever matches status = 'completed': a
// rejected/failed/pending transfer never moved money, so from the
// recipient's perspective it never happened — showing it would leak the
// sender's own fraud-rule trigger and unrelated activity to someone who
// was never party to a real event. The sender branch has no status filter:
// it's the sender's own transfer, in whatever state it's in.
const transferHistoryQueryFirstPage = `
(SELECT ` + transferHistoryColumns + `
 FROM transfers
 WHERE sender_account_id = $1
 ORDER BY created_at DESC, id DESC
 LIMIT $2)
UNION ALL
(SELECT ` + transferHistoryColumns + `
 FROM transfers
 WHERE recipient_account_id = $1 AND status = 'completed'
 ORDER BY created_at DESC, id DESC
 LIMIT $2)
ORDER BY created_at DESC, id DESC
LIMIT $2`

const transferHistoryQueryNextPage = `
(SELECT ` + transferHistoryColumns + `
 FROM transfers
 WHERE sender_account_id = $1 AND (created_at, id) < ($3, $4)
 ORDER BY created_at DESC, id DESC
 LIMIT $2)
UNION ALL
(SELECT ` + transferHistoryColumns + `
 FROM transfers
 WHERE recipient_account_id = $1 AND status = 'completed' AND (created_at, id) < ($3, $4)
 ORDER BY created_at DESC, id DESC
 LIMIT $2)
ORDER BY created_at DESC, id DESC
LIMIT $2`

// getTransferHistoryPage returns up to limit of accountID's transfers (as
// sender — any status — or as recipient — status = 'completed' only, see
// the query comment above), newest first, plus whether more rows exist
// beyond this page. cursor is nil for the first page; otherwise it's the
// (created_at, id) of the last row returned by the previous page.
//
// Internally requests limit+1 rows (one extra to detect "is there a next
// page" without a separate COUNT query), then trims back to limit.
func getTransferHistoryPage(ctx context.Context, pool *pgxpool.Pool, accountID string, limit int32, cursor *pageCursor) ([]Transfer, bool, error) {
	fetchLimit := limit + 1

	var rows pgx.Rows
	var err error
	if cursor == nil {
		rows, err = pool.Query(ctx, transferHistoryQueryFirstPage, accountID, fetchLimit)
	} else {
		rows, err = pool.Query(ctx, transferHistoryQueryNextPage, accountID, fetchLimit, cursor.CreatedAt, cursor.ID)
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if cursor != nil && errors.As(err, &pgErr) && pgErr.Code == invalidTextRepresentation {
			return nil, false, errInvalidCursor
		}
		return nil, false, fmt.Errorf("query transfer history: %w", err)
	}
	defer rows.Close()

	transfers := make([]Transfer, 0, fetchLimit)
	for rows.Next() {
		var t Transfer
		if err := rows.Scan(&t.ID, &t.IdempotencyKey, &t.SenderAccountID, &t.RecipientAccountID, &t.Amount, &t.Status, &t.FailureReason, &t.LedgerTransactionID, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, false, fmt.Errorf("scan transfer: %w", err)
		}
		transfers = append(transfers, t)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("query transfer history: %w", err)
	}

	hasMore := len(transfers) > int(limit)
	if hasMore {
		transfers = transfers[:limit]
	}
	return transfers, hasMore, nil
}

// reconcileReplay compares an existing transfer found by idempotency key
// (via the fast path, or because this request lost the insert race) against
// the CURRENT request's parameters. A mismatch means the key was reused for
// a different transfer — a client bug — and must not silently return
// someone else's transfer state. recipientIban is resolved fresh (a cheap,
// read-only, side-effect-free call) purely to obtain the account_id to
// compare, not to re-run any business validation — those only apply to a
// genuinely new transfer, not a replay. Any resolve error here (rate
// limited, invalid IBAN, wrong bank, not found, or a real transport
// failure) is treated uniformly as a generic error, unlike createTransfer's
// switch over specific outcomes: those specific outcomes describe a NEW
// transfer's validation, and none of them make sense to report for a
// replay of a request that already succeeded once.
func reconcileReplay(ctx context.Context, accountsClient accountsv1.AccountsServiceClient, existing Transfer, senderAccountID, userID, recipientIban string, amount int64) (Transfer, createTransferOutcome, error) {
	if existing.SenderAccountID != senderAccountID || existing.Amount != amount {
		return Transfer{}, createTransferKeyReused, nil
	}
	recipient, err := accountsClient.ResolveAccountByIban(ctx, &accountsv1.ResolveAccountByIbanRequest{Iban: recipientIban, UserId: userID})
	if err != nil {
		return Transfer{}, 0, fmt.Errorf("resolve recipient account for idempotency check: %w", err)
	}
	if recipient.GetAccountId() != existing.RecipientAccountID {
		return Transfer{}, createTransferKeyReused, nil
	}
	return existing, createTransferReplayed, nil
}

type settlementOutcome int

const (
	settlementCompleted settlementOutcome = iota
	settlementFailed
	settlementUncertain
)

// settleTransfer calls ledger-svc.ExecuteTransfer for an already-created
// pending transfer and updates the row to a DEFINITE outcome (completed or
// failed) whenever ledger-svc actually answered.
//
// ledger-svc's ExecuteTransfer (services/ledger-svc/ledger.go) wraps its
// work in a single Postgres transaction that either fully commits or fully
// rolls back before any gRPC response is sent — so any well-formed response
// (success, or one of its own explicit status.Error(...) codes below) tells
// us for certain whether the money moved. codes.Unavailable,
// codes.DeadlineExceeded, and codes.Unknown are different in kind: ledger-svc
// itself never returns them — they arise only from the transport layer
// (couldn't reach it, or gave up waiting) — meaning we genuinely don't know
// whether the request ever reached ledger-svc, or reached it and committed
// but the response was lost on the way back. Marking the transfer failed
// there could contradict a real debit; marking it completed without a
// transaction_id would be a lie. So it stays pending, unchanged. See
// README's "Money transfer through the ledger" section for the full
// boundary this leaves — real resolution needs reconciliation against
// ledger-svc (e.g. via GetHistory, or a request id ledger-svc doesn't
// yet accept), out of scope here.
func settleTransfer(ctx context.Context, pool *pgxpool.Pool, ledgerClient ledgerv1.LedgerServiceClient, transfer Transfer) (Transfer, settlementOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, ledgerCallTimeout)
	defer cancel()

	resp, err := ledgerClient.ExecuteTransfer(ctx, &ledgerv1.ExecuteTransferRequest{
		FromAccountId: transfer.SenderAccountID,
		ToAccountId:   transfer.RecipientAccountID,
		Amount:        transfer.Amount,
		// Tags both entries this call writes with this transfer's own id,
		// so the reconciliation worker (reconcile.go) can later ask
		// ledger-svc's GetTransactionByReference whether this specific
		// call actually committed — needed for exactly the case this
		// function's own doc comment describes: a transport failure where
		// we can't tell whether the request landed.
		Reference: transfer.ID,
	})
	if err == nil {
		updated, dbErr := markTransferCompleted(ctx, pool, transfer.ID, resp.GetTransactionId())
		if dbErr != nil {
			return Transfer{}, 0, dbErr
		}
		return updated, settlementCompleted, nil
	}

	var failureReason string
	switch status.Code(err) {
	case codes.FailedPrecondition:
		failureReason = failureReasonInsufficientFunds
	case codes.NotFound:
		failureReason = failureReasonAccountNotFound
	case codes.InvalidArgument:
		failureReason = failureReasonInvalidAmount
	case codes.Internal:
		failureReason = failureReasonLedgerInternal
	default:
		// codes.Unavailable, codes.DeadlineExceeded, codes.Unknown: we do
		// not know whether the transfer executed. Leave the row exactly as
		// it is (pending) — do not write anything.
		return transfer, settlementUncertain, nil
	}

	updated, dbErr := markTransferFailed(ctx, pool, transfer.ID, failureReason)
	if dbErr != nil {
		return Transfer{}, 0, dbErr
	}
	return updated, settlementFailed, nil
}

// markTransferCompleted and markTransferFailed don't distinguish sender vs.
// recipient on a ledger NotFound (ledger-svc's message text does — "from
// account not found" vs "to account not found" — but that's a string, not a
// structured detail). Both accounts are already validated to exist by
// createTransfer, so this path should be unreachable in practice; a single
// generic account_not_found reason is enough rather than parsing
// ledger-svc's message wording, a fragile coupling.

// markTransferCompleted, markTransferFailed, and markTransferRejected each
// write their status UPDATE and the corresponding outbox event in a single
// Postgres transaction — either both persist or neither does. This is the
// entire point of the outbox pattern (see auth-svc's kafka.go TODO for the
// dual-write gap it closes): a transfer must never reach a terminal status
// without its event durably queued for a later Kafka publish, and an event
// must never exist for a status change that got rolled back.
func markTransferCompleted(ctx context.Context, pool *pgxpool.Pool, id, ledgerTransactionID string) (Transfer, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Transfer{}, fmt.Errorf("mark transfer completed: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var t Transfer
	err = tx.QueryRow(ctx,
		`UPDATE transfers SET status = 'completed', ledger_transaction_id = $1, updated_at = now()
		 WHERE id = $2
		 RETURNING id, idempotency_key, sender_account_id, recipient_account_id, amount, status, failure_reason, ledger_transaction_id, created_at, updated_at`,
		ledgerTransactionID, id,
	).Scan(&t.ID, &t.IdempotencyKey, &t.SenderAccountID, &t.RecipientAccountID, &t.Amount, &t.Status, &t.FailureReason, &t.LedgerTransactionID, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return Transfer{}, fmt.Errorf("mark transfer completed: %w", err)
	}

	eventID, err := outbox.GenerateEventID()
	if err != nil {
		return Transfer{}, fmt.Errorf("mark transfer completed: generate event id: %w", err)
	}
	payload, err := proto.Marshal(&eventsv1.TransferCompleted{
		EventId:             eventID,
		TransferId:          t.ID,
		SenderAccountId:     t.SenderAccountID,
		RecipientAccountId:  t.RecipientAccountID,
		Amount:              t.Amount,
		LedgerTransactionId: ledgerTransactionID,
		OccurredAt:          timestamppb.New(time.Now()),
	})
	if err != nil {
		return Transfer{}, fmt.Errorf("mark transfer completed: marshal event: %w", err)
	}
	if err := outbox.InsertEvent(ctx, tx, outboxTable, eventID, "TransferCompleted", t.SenderAccountID, payload); err != nil {
		return Transfer{}, fmt.Errorf("mark transfer completed: insert outbox event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Transfer{}, fmt.Errorf("mark transfer completed: commit tx: %w", err)
	}
	return t, nil
}

func markTransferFailed(ctx context.Context, pool *pgxpool.Pool, id, failureReason string) (Transfer, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Transfer{}, fmt.Errorf("mark transfer failed: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var t Transfer
	err = tx.QueryRow(ctx,
		`UPDATE transfers SET status = 'failed', failure_reason = $1, updated_at = now()
		 WHERE id = $2
		 RETURNING id, idempotency_key, sender_account_id, recipient_account_id, amount, status, failure_reason, ledger_transaction_id, created_at, updated_at`,
		failureReason, id,
	).Scan(&t.ID, &t.IdempotencyKey, &t.SenderAccountID, &t.RecipientAccountID, &t.Amount, &t.Status, &t.FailureReason, &t.LedgerTransactionID, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return Transfer{}, fmt.Errorf("mark transfer failed: %w", err)
	}

	eventID, err := outbox.GenerateEventID()
	if err != nil {
		return Transfer{}, fmt.Errorf("mark transfer failed: generate event id: %w", err)
	}
	payload, err := proto.Marshal(&eventsv1.TransferFailed{
		EventId:            eventID,
		TransferId:         t.ID,
		SenderAccountId:    t.SenderAccountID,
		RecipientAccountId: t.RecipientAccountID,
		Amount:             t.Amount,
		Reason:             failureReason,
		OccurredAt:         timestamppb.New(time.Now()),
	})
	if err != nil {
		return Transfer{}, fmt.Errorf("mark transfer failed: marshal event: %w", err)
	}
	if err := outbox.InsertEvent(ctx, tx, outboxTable, eventID, "TransferFailed", t.SenderAccountID, payload); err != nil {
		return Transfer{}, fmt.Errorf("mark transfer failed: insert outbox event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Transfer{}, fmt.Errorf("mark transfer failed: commit tx: %w", err)
	}
	return t, nil
}

// getStalePendingTransfers returns transfers stuck in 'pending' for longer
// than staleAfter — candidates for reconcile.go's worker to resolve
// against ledger-svc's own data. A transfer that's merely pending because
// settleTransfer hasn't been called yet at all (createTransferOK, fraud
// check still running) is never this old within the same request, so this
// threshold naturally excludes in-flight requests without needing to
// distinguish them explicitly.
func getStalePendingTransfers(ctx context.Context, pool *pgxpool.Pool, staleAfter time.Duration) ([]Transfer, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, idempotency_key, sender_account_id, recipient_account_id, amount, status, failure_reason, ledger_transaction_id, created_at, updated_at
		 FROM transfers
		 WHERE status = 'pending' AND updated_at < now() - make_interval(secs => $1)`,
		staleAfter.Seconds(),
	)
	if err != nil {
		return nil, fmt.Errorf("query stale pending transfers: %w", err)
	}
	defer rows.Close()

	var transfers []Transfer
	for rows.Next() {
		var t Transfer
		if err := rows.Scan(&t.ID, &t.IdempotencyKey, &t.SenderAccountID, &t.RecipientAccountID, &t.Amount, &t.Status, &t.FailureReason, &t.LedgerTransactionID, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan stale pending transfer: %w", err)
		}
		transfers = append(transfers, t)
	}
	return transfers, rows.Err()
}

// markTransferCompletedIfPending and markTransferFailedIfPending are
// reconcile.go's writers, not settleTransfer's — they add "AND status =
// 'pending'" to the same UPDATEs markTransferCompleted/markTransferFailed
// already do. This is the race guard requirement 3 of the reconciliation
// task calls for: the worker reads a transfer as stale-pending, but by the
// time it writes, a concurrent ordinary request for that same transfer
// (also possible if, say, the client retried and this time reached
// settleTransfer for real) may have already resolved it. Without the
// status condition, the worker's write could silently overwrite an
// already-committed result with stale information. The returned bool
// reports whether this call's write actually happened, so the caller can
// tell "resolved it" apart from "someone else already had."
//
// Both take the full transfer (rather than just its id) because the
// outbox event needs sender_account_id/recipient_account_id/amount, which
// are immutable after creation and already sit on the caller's Transfer —
// no extra RETURNING/scan is needed to get them. When the race is lost
// (RowsAffected == 0), nothing is written to outbox either: whoever
// resolved the transfer first is responsible for its own event.
func markTransferCompletedIfPending(ctx context.Context, pool *pgxpool.Pool, transfer Transfer, ledgerTransactionID string) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("mark transfer completed if pending: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE transfers SET status = 'completed', ledger_transaction_id = $1, updated_at = now()
		 WHERE id = $2 AND status = 'pending'`,
		ledgerTransactionID, transfer.ID,
	)
	if err != nil {
		return false, fmt.Errorf("mark transfer completed if pending: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}

	eventID, err := outbox.GenerateEventID()
	if err != nil {
		return false, fmt.Errorf("mark transfer completed if pending: generate event id: %w", err)
	}
	payload, err := proto.Marshal(&eventsv1.TransferCompleted{
		EventId:             eventID,
		TransferId:          transfer.ID,
		SenderAccountId:     transfer.SenderAccountID,
		RecipientAccountId:  transfer.RecipientAccountID,
		Amount:              transfer.Amount,
		LedgerTransactionId: ledgerTransactionID,
		OccurredAt:          timestamppb.New(time.Now()),
	})
	if err != nil {
		return false, fmt.Errorf("mark transfer completed if pending: marshal event: %w", err)
	}
	if err := outbox.InsertEvent(ctx, tx, outboxTable, eventID, "TransferCompleted", transfer.SenderAccountID, payload); err != nil {
		return false, fmt.Errorf("mark transfer completed if pending: insert outbox event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("mark transfer completed if pending: commit tx: %w", err)
	}
	return true, nil
}

func markTransferFailedIfPending(ctx context.Context, pool *pgxpool.Pool, transfer Transfer, failureReason string) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("mark transfer failed if pending: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE transfers SET status = 'failed', failure_reason = $1, updated_at = now()
		 WHERE id = $2 AND status = 'pending'`,
		failureReason, transfer.ID,
	)
	if err != nil {
		return false, fmt.Errorf("mark transfer failed if pending: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}

	eventID, err := outbox.GenerateEventID()
	if err != nil {
		return false, fmt.Errorf("mark transfer failed if pending: generate event id: %w", err)
	}
	payload, err := proto.Marshal(&eventsv1.TransferFailed{
		EventId:            eventID,
		TransferId:         transfer.ID,
		SenderAccountId:    transfer.SenderAccountID,
		RecipientAccountId: transfer.RecipientAccountID,
		Amount:             transfer.Amount,
		Reason:             failureReason,
		OccurredAt:         timestamppb.New(time.Now()),
	})
	if err != nil {
		return false, fmt.Errorf("mark transfer failed if pending: marshal event: %w", err)
	}
	if err := outbox.InsertEvent(ctx, tx, outboxTable, eventID, "TransferFailed", transfer.SenderAccountID, payload); err != nil {
		return false, fmt.Errorf("mark transfer failed if pending: insert outbox event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("mark transfer failed if pending: commit tx: %w", err)
	}
	return true, nil
}

type fraudCheckOutcome int

const (
	fraudCheckApproved fraudCheckOutcome = iota
	fraudCheckRejected
	fraudCheckUncertain
)

// String — same reasoning as createTransferOutcome.String.
func (o fraudCheckOutcome) String() string {
	switch o {
	case fraudCheckApproved:
		return "approved"
	case fraudCheckRejected:
		return "rejected"
	case fraudCheckUncertain:
		return "uncertain"
	default:
		return "unknown"
	}
}

// checkTransferFraud calls fraud-svc for an already-created pending transfer
// and, on a definite reject, writes the terminal 'rejected' status itself —
// mirroring settleTransfer's shape (call + write the terminal state on a
// definite answer). It must only ever be called for a freshly created
// transfer, never for a replayed one (see the call site in http.go): a
// replay must not run fraud-svc a second time, both to avoid double-charging
// an external check and because a repeated approve/reject call would itself
// feed fraud-svc's own velocity counters.
//
// fraud-svc's contract (services/fraud-svc/grpc_server.go) has exactly one
// error meaning today: codes.Internal means scoring failed, full stop —
// there is no ledger-style set of business-error codes to switch on, so any
// error at all here means "couldn't get an answer," not a specific business
// outcome. This project fails closed: an unreachable/erroring fraud-svc must
// not silently approve, so the transfer stays pending, untouched, exactly
// like settleTransfer's own transport-failure branch — a definite decision
// is never guessed at.
func checkTransferFraud(ctx context.Context, pool *pgxpool.Pool, fraudClient fraudv1.FraudServiceClient, transfer Transfer) (Transfer, fraudCheckOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, fraudCallTimeout)
	defer cancel()

	resp, err := fraudClient.CheckTransfer(ctx, &fraudv1.CheckTransferRequest{
		TransferId: transfer.ID,
		AccountId:  transfer.SenderAccountID,
		Amount:     transfer.Amount,
	})
	if err != nil {
		// Do not know whether fraud-svc would have approved or rejected —
		// leave the row exactly as it is (pending), same as
		// settleTransfer's transport-failure branch.
		return transfer, fraudCheckUncertain, nil
	}

	switch resp.GetDecision() {
	case "approve":
		return transfer, fraudCheckApproved, nil
	case "reject":
		updated, dbErr := markTransferRejected(ctx, pool, transfer.ID, resp.GetTriggeredRule())
		if dbErr != nil {
			return Transfer{}, 0, dbErr
		}
		return updated, fraudCheckRejected, nil
	default:
		// A contract/version-skew bug (fraud-svc returned neither "approve"
		// nor "reject"), not a real outcome — worth operator visibility,
		// unlike a plain transport failure. Fail closed rather than guess.
		log.Printf("transfers-svc: checkTransferFraud: unexpected decision %q for transfer %s", resp.GetDecision(), transfer.ID)
		return transfer, fraudCheckUncertain, nil
	}
}

// nullableTraceContext renders the caller's trace for storage on a
// resource row, or NULL when there is no trace to record.
//
// Written at creation because that is the only moment the originating
// request's context exists. The reconciliation workers that read it back
// run minutes later on a timer, with no parent span of their own — see
// originTraceContext.
func nullableTraceContext(ctx context.Context) *string {
	if serialized := tracing.SerializeContext(ctx); serialized != "" {
		return &serialized
	}
	return nil
}

// originTraceContext reads back the trace recorded when a row was
// created, so a background worker can link its work to the request that
// caused it.
//
// A separate one-row query rather than an extra column on every Transfer
// and Deposit scan in this service: only the reconciliation paths ever
// need it, they handle a handful of rows per tick, and threading a
// field through a dozen unrelated SELECTs to serve two call sites would
// cost more clarity than the query costs time.
//
// table is a hardcoded literal at both call sites ("transfers",
// "deposits"), never anything derived from input — same rule as
// pkg/outbox's table parameter.
func originTraceContext(ctx context.Context, pool *pgxpool.Pool, table, id string) string {
	var serialized string
	err := pool.QueryRow(ctx,
		fmt.Sprintf("SELECT COALESCE(trace_context::text, '') FROM %s WHERE id = $1", table), id,
	).Scan(&serialized)
	if err != nil {
		// Not worth failing reconciliation over: the work still gets
		// done, the span just roots its own trace with no link back.
		return ""
	}
	return serialized
}

func markTransferRejected(ctx context.Context, pool *pgxpool.Pool, id, triggeredRule string) (Transfer, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Transfer{}, fmt.Errorf("mark transfer rejected: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var t Transfer
	err = tx.QueryRow(ctx,
		`UPDATE transfers SET status = 'rejected', failure_reason = $1, updated_at = now()
		 WHERE id = $2
		 RETURNING id, idempotency_key, sender_account_id, recipient_account_id, amount, status, failure_reason, ledger_transaction_id, created_at, updated_at`,
		triggeredRule, id,
	).Scan(&t.ID, &t.IdempotencyKey, &t.SenderAccountID, &t.RecipientAccountID, &t.Amount, &t.Status, &t.FailureReason, &t.LedgerTransactionID, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return Transfer{}, fmt.Errorf("mark transfer rejected: %w", err)
	}

	eventID, err := outbox.GenerateEventID()
	if err != nil {
		return Transfer{}, fmt.Errorf("mark transfer rejected: generate event id: %w", err)
	}
	payload, err := proto.Marshal(&eventsv1.TransferRejected{
		EventId:            eventID,
		TransferId:         t.ID,
		SenderAccountId:    t.SenderAccountID,
		RecipientAccountId: t.RecipientAccountID,
		Amount:             t.Amount,
		TriggeredRule:      triggeredRule,
		OccurredAt:         timestamppb.New(time.Now()),
	})
	if err != nil {
		return Transfer{}, fmt.Errorf("mark transfer rejected: marshal event: %w", err)
	}
	if err := outbox.InsertEvent(ctx, tx, outboxTable, eventID, "TransferRejected", t.SenderAccountID, payload); err != nil {
		return Transfer{}, fmt.Errorf("mark transfer rejected: insert outbox event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Transfer{}, fmt.Errorf("mark transfer rejected: commit tx: %w", err)
	}
	return t, nil
}
