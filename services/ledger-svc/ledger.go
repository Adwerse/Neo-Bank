package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrLedgerAccountNotFound indicates no ledger_accounts row exists for the
// given account_id. It is distinct from a genuinely zero balance: an
// account that exists but has no entries (or entries summing to zero) is a
// valid state, not an error.
var ErrLedgerAccountNotFound = errors.New("ledger account not found")

// ErrInvalidPagination indicates a non-positive limit or negative offset
// was passed to getHistory.
var ErrInvalidPagination = errors.New("invalid pagination")

// getBalance reads accountID's balance from account_balances, a cache/
// projection of the entries log kept up to date by executeTransfer (and
// repairable from the log at any time via rebuildBalance). The entries log
// remains the sole source of truth — account_balances just avoids an O(n)
// SUM over potentially thousands of entries on every read. A missing
// account_balances row (an account that exists but has never been the
// target of a balance-affecting write) is a valid zero balance, not an
// error, matching the old SUM-based behavior for zero-entry accounts.
func getBalance(ctx context.Context, pool *pgxpool.Pool, accountID string) (int64, error) {
	var balance int64
	err := pool.QueryRow(ctx,
		`SELECT COALESCE(ab.balance, 0)
		 FROM ledger_accounts la
		 LEFT JOIN account_balances ab ON ab.ledger_account_id = la.id
		 WHERE la.account_id = $1`,
		accountID,
	).Scan(&balance)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrLedgerAccountNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("look up account balance: %w", err)
	}
	return balance, nil
}

// LedgerAccount is a ledger_accounts row, as returned by createLedgerAccount.
type LedgerAccount struct {
	AccountID string
	CreatedAt time.Time
}

// createLedgerAccount gets-or-creates the ledger_accounts row for accountID
// and returns it. It is idempotent: a second call for the same account_id
// returns the existing row rather than erroring, relying on the
// account_id UNIQUE constraint. This is the same upsert as the seed tool's
// ensureLedgerAccount (cmd/seed) — the ON CONFLICT ... DO UPDATE is a no-op
// write (account_id rewritten to itself) purely so RETURNING still yields
// the existing row on a repeat call, where DO NOTHING would return no rows.
//
// Idempotency matters because the caller (accounts-svc) invokes this off an
// at-least-once Kafka event that can be redelivered: a redelivery must be a
// safe no-op, not a duplicate-key error.
func createLedgerAccount(ctx context.Context, pool *pgxpool.Pool, accountID string) (LedgerAccount, error) {
	var acc LedgerAccount
	err := pool.QueryRow(ctx,
		`INSERT INTO ledger_accounts (account_id)
		 VALUES ($1)
		 ON CONFLICT (account_id) DO UPDATE SET account_id = EXCLUDED.account_id
		 RETURNING account_id, created_at`,
		accountID,
	).Scan(&acc.AccountID, &acc.CreatedAt)
	if err != nil {
		return LedgerAccount{}, fmt.Errorf("upsert ledger account: %w", err)
	}
	return acc, nil
}

// LedgerEntry is one row of an account's entries log, as returned by
// getHistory.
type LedgerEntry struct {
	TransactionID string
	Amount        int64
	CreatedAt     time.Time
}

// getHistory returns accountID's entries, newest first, paginated via
// limit/offset — the log grows without bound, so an unpaginated read would
// eventually scan an account's entire history on every call. Ties in
// created_at are broken by id: both entries of a single executeTransfer
// share the same timestamp (Postgres's now() is fixed for the duration of
// a transaction), so created_at alone isn't a stable enough sort key to
// paginate over without risking skipped or duplicated rows across pages.
func getHistory(ctx context.Context, pool *pgxpool.Pool, accountID string, limit, offset int32) ([]LedgerEntry, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: limit must be positive, got %d", ErrInvalidPagination, limit)
	}
	if offset < 0 {
		return nil, fmt.Errorf("%w: offset must be non-negative, got %d", ErrInvalidPagination, offset)
	}

	var ledgerAccountID string
	err := pool.QueryRow(ctx,
		"SELECT id FROM ledger_accounts WHERE account_id = $1",
		accountID,
	).Scan(&ledgerAccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrLedgerAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("look up ledger account: %w", err)
	}

	rows, err := pool.Query(ctx,
		`SELECT transaction_id, amount, created_at FROM entries
		 WHERE ledger_account_id = $1
		 ORDER BY created_at DESC, id DESC
		 LIMIT $2 OFFSET $3`,
		ledgerAccountID, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("query entries: %w", err)
	}
	defer rows.Close()

	entries := make([]LedgerEntry, 0, limit)
	for rows.Next() {
		var e LedgerEntry
		if err := rows.Scan(&e.TransactionID, &e.Amount, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan entry: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate entries: %w", err)
	}
	return entries, nil
}

// BalancePoint is one day's closing balance, oldest first, as returned by
// getBalanceHistory.
type BalancePoint struct {
	Date    time.Time
	Balance int64
}

// getBalanceHistory returns accountID's daily closing balance from from
// (inclusive) to now, oldest first, for charting — already aggregated
// server-side so a caller never has to walk the raw entries log itself.
// from nil means "from the beginning of history."
//
// When from is set, the first returned point is not "the first day
// something happened" but the balance carried INTO the window, dated at
// from (day-truncated) — so a chart never has to guess what the balance
// was before its own first visible entry. When from is nil there is no
// such anchor: a ledger account's balance is definitionally 0 before its
// first entry, so the real data already starts at the right place.
//
// A single query handles both the bounded and unbounded case (pgx binds a
// nil *time.Time as SQL NULL) rather than branching between two SQL
// strings, matching this file's existing no-dynamic-SQL style.
func getBalanceHistory(ctx context.Context, pool *pgxpool.Pool, accountID string, from *time.Time) ([]BalancePoint, error) {
	var ledgerAccountID string
	err := pool.QueryRow(ctx,
		"SELECT id FROM ledger_accounts WHERE account_id = $1",
		accountID,
	).Scan(&ledgerAccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrLedgerAccountNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("look up ledger account: %w", err)
	}

	var balanceBefore int64
	if from != nil {
		err = pool.QueryRow(ctx,
			"SELECT COALESCE(SUM(amount), 0)::bigint FROM entries WHERE ledger_account_id = $1 AND created_at < $2",
			ledgerAccountID, *from,
		).Scan(&balanceBefore)
		if err != nil {
			return nil, fmt.Errorf("sum entries before range: %w", err)
		}
	}

	rows, err := pool.Query(ctx,
		`SELECT date_trunc('day', created_at AT TIME ZONE 'UTC')::date AS day, SUM(amount)::bigint AS delta
		 FROM entries
		 WHERE ledger_account_id = $1 AND ($2::timestamptz IS NULL OR created_at >= $2)
		 GROUP BY 1
		 ORDER BY 1`,
		ledgerAccountID, from,
	)
	if err != nil {
		return nil, fmt.Errorf("query daily deltas: %w", err)
	}
	defer rows.Close()

	points := make([]BalancePoint, 0)
	if from != nil {
		points = append(points, BalancePoint{Date: from.Truncate(24 * time.Hour), Balance: balanceBefore})
	}
	running := balanceBefore
	for rows.Next() {
		var day time.Time
		var delta int64
		if err := rows.Scan(&day, &delta); err != nil {
			return nil, fmt.Errorf("scan daily delta: %w", err)
		}
		running += delta
		points = append(points, BalancePoint{Date: day, Balance: running})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate daily deltas: %w", err)
	}
	return points, nil
}

// rebuildBalance recomputes accountID's balance from the entries log —
// the source of truth — and overwrites account_balances with the result,
// fixing any drift between the cache and the log. It locks the ledger
// account FOR UPDATE first, same as executeTransfer, so it can't race a
// concurrent transfer: either it runs before the transfer's lock is
// acquired (and sees a balance the transfer will then correctly update),
// or after the transfer commits (and its SUM(entries) already reflects
// the transfer's entries).
func rebuildBalance(ctx context.Context, pool *pgxpool.Pool, accountID string) (int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	ledgerAccountID, found, err := lockLedgerAccount(ctx, tx, accountID)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, ErrLedgerAccountNotFound
	}

	var balance int64
	err = tx.QueryRow(ctx,
		"SELECT COALESCE(SUM(amount), 0)::bigint FROM entries WHERE ledger_account_id = $1",
		ledgerAccountID,
	).Scan(&balance)
	if err != nil {
		return 0, fmt.Errorf("sum entries: %w", err)
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO account_balances (ledger_account_id, balance, updated_at)
		 VALUES ($1, $2, now())
		 ON CONFLICT (ledger_account_id)
		 DO UPDATE SET balance = EXCLUDED.balance, updated_at = EXCLUDED.updated_at`,
		ledgerAccountID, balance,
	)
	if err != nil {
		return 0, fmt.Errorf("write rebuilt balance: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit rebuild: %w", err)
	}
	return balance, nil
}

// transferOutcome distinguishes the business-level results of
// executeTransfer. Only genuinely unexpected failures (a lost DB
// connection, a malformed query) are reported via the error return instead;
// every outcome below is an expected, non-exceptional branch.
type transferOutcome int

const (
	transferOK transferOutcome = iota
	transferInvalidAmount
	transferFromAccountNotFound
	transferToAccountNotFound
	transferInsufficientFunds
)

// String renders the outcome for span attributes. Explicit strings rather
// than the iota's number, so a trace reads "insufficient_funds" instead of
// "4" — and so inserting a constant anywhere but the end cannot silently
// re-label traces already stored in Jaeger.
func (o transferOutcome) String() string {
	switch o {
	case transferOK:
		return "posted"
	case transferInvalidAmount:
		return "invalid_amount"
	case transferFromAccountNotFound:
		return "from_account_not_found"
	case transferToAccountNotFound:
		return "to_account_not_found"
	case transferInsufficientFunds:
		return "insufficient_funds"
	default:
		return "unknown"
	}
}

// lockLedgerAccount looks up and FOR-UPDATE-locks the ledger_accounts row
// for accountID within tx. Callers transferring funds between two accounts
// must lock both sides in ascending account_id order (see executeTransfer)
// so that two concurrent transfers running in opposite directions between
// the same pair of accounts always attempt to lock the same row first,
// rather than deadlocking on each other.
func lockLedgerAccount(ctx context.Context, tx pgx.Tx, accountID string) (ledgerAccountID string, found bool, err error) {
	err = tx.QueryRow(ctx,
		"SELECT id FROM ledger_accounts WHERE account_id = $1 FOR UPDATE",
		accountID,
	).Scan(&ledgerAccountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("lock ledger account: %w", err)
	}
	return ledgerAccountID, true, nil
}

// executeTransfer atomically moves amount from fromAccountID to
// toAccountID by inserting a balanced debit/credit pair of entries sharing
// one transaction_id. The global SUM(entries) = 0 invariant holds
// automatically because the pair always nets to zero; the "no account goes
// negative" invariant is enforced by checking fromAccountID's balance
// inside the same transaction that inserts the entries.
//
// Concurrency: both accounts are locked FOR UPDATE, in ascending
// account_id order, before the balance check. Locking prevents the race
// where two concurrent transfers both read the same stale balance and
// jointly overdraw the account — the second transfer's lock acquisition
// blocks until the first commits or rolls back, so it always evaluates the
// balance check against up-to-date state. Locking in a fixed order
// (independent of transfer direction) prevents deadlock between two
// concurrent transfers going in opposite directions between the same pair
// of accounts, since both will always attempt to lock the same account
// first rather than each holding one lock and waiting on the other.
func executeTransfer(ctx context.Context, pool *pgxpool.Pool, fromAccountID, toAccountID string, amount int64, reference string) (transactionID string, outcome transferOutcome, err error) {
	if amount <= 0 {
		return "", transferInvalidAmount, nil
	}

	// A nil interface{} binds as SQL NULL for the (nullable) reference
	// column; an empty string would fail Postgres's uuid input parsing
	// instead. Most callers pass no reference at all (devtopup, cmd/seed) —
	// this keeps their entries exactly as before.
	var referenceParam any
	if reference != "" {
		referenceParam = reference
	}

	first, second := fromAccountID, toAccountID
	if strings.ToLower(toAccountID) < strings.ToLower(fromAccountID) {
		first, second = toAccountID, fromAccountID
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", 0, err
	}
	defer tx.Rollback(ctx)

	locked := make(map[string]string, 2) // account_id -> ledger_accounts.id
	for _, accountID := range [2]string{first, second} {
		ledgerAccountID, found, lockErr := lockLedgerAccount(ctx, tx, accountID)
		if lockErr != nil {
			return "", 0, lockErr
		}
		if !found {
			if accountID == fromAccountID {
				return "", transferFromAccountNotFound, nil
			}
			return "", transferToAccountNotFound, nil
		}
		locked[accountID] = ledgerAccountID
	}
	fromLedgerAccountID := locked[fromAccountID]
	toLedgerAccountID := locked[toAccountID]

	var fromBalance int64
	err = tx.QueryRow(ctx,
		"SELECT COALESCE(SUM(amount), 0)::bigint FROM entries WHERE ledger_account_id = $1",
		fromLedgerAccountID,
	).Scan(&fromBalance)
	if err != nil {
		return "", 0, fmt.Errorf("sum entries: %w", err)
	}
	if fromBalance < amount {
		return "", transferInsufficientFunds, nil
	}

	err = tx.QueryRow(ctx,
		`INSERT INTO entries (transaction_id, ledger_account_id, amount, reference)
		 VALUES (gen_random_uuid(), $1, $2, $3)
		 RETURNING transaction_id`,
		fromLedgerAccountID, -amount, referenceParam,
	).Scan(&transactionID)
	if err != nil {
		return "", 0, fmt.Errorf("insert debit entry: %w", err)
	}

	_, err = tx.Exec(ctx,
		"INSERT INTO entries (transaction_id, ledger_account_id, amount, reference) VALUES ($1, $2, $3, $4)",
		transactionID, toLedgerAccountID, amount, referenceParam,
	)
	if err != nil {
		return "", 0, fmt.Errorf("insert credit entry: %w", err)
	}

	if err := applyBalanceDelta(ctx, tx, fromLedgerAccountID, -amount); err != nil {
		return "", 0, err
	}
	if err := applyBalanceDelta(ctx, tx, toLedgerAccountID, amount); err != nil {
		return "", 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return "", 0, fmt.Errorf("commit transfer: %w", err)
	}
	return transactionID, transferOK, nil
}

// genesisAccountID is the fixed account_id of the system's genesis ledger
// account, provisioned deterministically by migration
// 000005_create_genesis_ledger_account (both id and account_id pinned to
// this same UUID there). It is the same constant independently hardcoded
// as genesisAccountID in cmd/seed/main.go and cmd/devtopup/main.go — that
// duplication across the three main packages is consistent with the
// existing pattern in those two files, not a new problem introduced here.
const genesisAccountID = "00000000-0000-0000-0000-000000000001"

// depositOutcome distinguishes the business-level results of deposit and
// reverseDeposit, mirroring transferOutcome. There is deliberately no
// insufficient-funds outcome: postUncheckedTransfer never checks either
// side's balance — see its doc comment.
type depositOutcome int

const (
	depositOK depositOutcome = iota
	depositInvalidAmount
	depositAccountNotFound
)

// deposit atomically credits amount into toAccountID from the genesis
// account — the ledger-side counterpart of an external, Stripe-funded
// top-up. Money entering the system from outside is represented as
// genesis's balance going negative by exactly amount, the standard
// double-entry treatment of issuance (the same principle cmd/seed and
// cmd/devtopup already apply by hand via direct SQL). See
// postUncheckedTransfer for the shared mechanics and idempotency
// contract.
func deposit(ctx context.Context, pool *pgxpool.Pool, toAccountID string, amount int64, reference string) (transactionID string, outcome depositOutcome, err error) {
	return postUncheckedTransfer(ctx, pool, genesisAccountID, toAccountID, amount, reference)
}

// reverseDeposit atomically debits amount from fromAccountID back to the
// genesis account — the ledger-side counterpart of a Stripe refund on a
// deposit that was already credited. Unlike a normal withdrawal
// (ExecuteTransfer, which enforces the source account can't go negative),
// this deliberately does NOT check fromAccountID's balance: Stripe has
// already taken the money back regardless of what the user currently
// holds in-ledger (they may have already spent it via other transfers),
// and the books must reflect that external fact. A user account going
// negative as a result is a known, accepted MVP limitation — see README —
// not a bug; a real bank's equivalent is the customer owing a debt, which
// is out of scope here. See postUncheckedTransfer for the shared
// mechanics and idempotency contract.
func reverseDeposit(ctx context.Context, pool *pgxpool.Pool, fromAccountID string, amount int64, reference string) (transactionID string, outcome depositOutcome, err error) {
	return postUncheckedTransfer(ctx, pool, fromAccountID, genesisAccountID, amount, reference)
}

// postUncheckedTransfer atomically posts amount from fromAccountID to
// toAccountID, sharing its locking and balanced-entries mechanics with
// executeTransfer (lockLedgerAccount, applyBalanceDelta, one Postgres
// transaction, a debit/credit pair sharing one transaction_id, the same
// reference-nil-if-empty handling) but, unlike executeTransfer, never
// checks either side's balance. It exists as a separate function rather
// than a code path through executeTransfer for one deliberate reason:
// executeTransfer unconditionally enforces "fromAccountID's balance must
// cover amount", and both of this function's two callers (deposit,
// reverseDeposit) need that check skipped for their own distinct reasons.
// Bolting either exemption onto executeTransfer would add a special case
// to the one function every ordinary transfer depends on — keeping this
// standalone leaves executeTransfer's tested behavior completely
// unchanged.
//
// Idempotent on reference, unlike executeTransfer: if an entry already
// exists for reference, that entry's transaction_id is returned instead
// of posting a second one. This matters here in a way it doesn't for
// executeTransfer, because both callers can genuinely be invoked more
// than once for the same logical operation — deposit by transfers-svc's
// crediting worker retrying a 'succeeded' deposit it hasn't yet observed
// as 'credited' locally, reverseDeposit by a redelivered charge.refunded
// webhook — and unlike a transfer (which has its own idempotency-key
// table one layer up, in transfers-svc's own transfers table), a deposit
// or reversal has no such layer above ledger-svc protecting it from a
// duplicate post. Concurrent calls sharing the same reference are
// serialized via a transaction-scoped advisory lock keyed on reference
// (auto-released at commit or rollback), so the check-then-insert below
// can't race two callers past it at once — reference has no unique
// database constraint to fall back on, since executeTransfer already
// legitimately writes two entries (debit and credit) sharing one
// reference value, so a naive UNIQUE constraint on the column isn't
// viable.
func postUncheckedTransfer(ctx context.Context, pool *pgxpool.Pool, fromAccountID, toAccountID string, amount int64, reference string) (transactionID string, outcome depositOutcome, err error) {
	if amount <= 0 {
		return "", depositInvalidAmount, nil
	}

	// Same NULL-vs-empty-string reasoning as executeTransfer: an empty
	// string fails Postgres's uuid input parsing, so a nil interface{}
	// (SQL NULL) is used instead when no reference is given.
	var referenceParam any
	if reference != "" {
		referenceParam = reference
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", 0, err
	}
	defer tx.Rollback(ctx)

	if reference != "" {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", reference); err != nil {
			return "", 0, fmt.Errorf("acquire idempotency lock for reference %s: %w", reference, err)
		}
		var existingTransactionID string
		err := tx.QueryRow(ctx, "SELECT transaction_id FROM entries WHERE reference = $1 LIMIT 1", reference).Scan(&existingTransactionID)
		if err == nil {
			// Idempotent replay: a transaction for this reference already
			// exists (from an earlier, possibly-interrupted call), so
			// return it as-is rather than posting a second one. tx is
			// rolled back via defer — nothing was written this call.
			return existingTransactionID, depositOK, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", 0, fmt.Errorf("check existing entry for reference %s: %w", reference, err)
		}
	}

	// Lock in the same deterministic ascending-account_id order as
	// executeTransfer, so this can never deadlock against a concurrent
	// executeTransfer/deposit/reverseDeposit call that also touches these
	// two accounts.
	first, second := fromAccountID, toAccountID
	if strings.ToLower(toAccountID) < strings.ToLower(fromAccountID) {
		first, second = toAccountID, fromAccountID
	}

	locked := make(map[string]string, 2) // account_id -> ledger_accounts.id
	for _, accountID := range [2]string{first, second} {
		ledgerAccountID, found, lockErr := lockLedgerAccount(ctx, tx, accountID)
		if lockErr != nil {
			return "", 0, lockErr
		}
		if !found {
			return "", depositAccountNotFound, nil
		}
		locked[accountID] = ledgerAccountID
	}
	fromLedgerAccountID := locked[fromAccountID]
	toLedgerAccountID := locked[toAccountID]

	// Deliberately no balance check here — the one behavioral difference
	// from executeTransfer. Callers (deposit, reverseDeposit) each have
	// their own reason fromAccountID is allowed to go arbitrarily
	// negative; see their doc comments.

	err = tx.QueryRow(ctx,
		`INSERT INTO entries (transaction_id, ledger_account_id, amount, reference)
		 VALUES (gen_random_uuid(), $1, $2, $3)
		 RETURNING transaction_id`,
		fromLedgerAccountID, -amount, referenceParam,
	).Scan(&transactionID)
	if err != nil {
		return "", 0, fmt.Errorf("insert debit entry: %w", err)
	}

	_, err = tx.Exec(ctx,
		"INSERT INTO entries (transaction_id, ledger_account_id, amount, reference) VALUES ($1, $2, $3, $4)",
		transactionID, toLedgerAccountID, amount, referenceParam,
	)
	if err != nil {
		return "", 0, fmt.Errorf("insert credit entry: %w", err)
	}

	if err := applyBalanceDelta(ctx, tx, fromLedgerAccountID, -amount); err != nil {
		return "", 0, err
	}
	if err := applyBalanceDelta(ctx, tx, toLedgerAccountID, amount); err != nil {
		return "", 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return "", 0, fmt.Errorf("commit transfer: %w", err)
	}
	return transactionID, depositOK, nil
}

// getTransactionByReference answers whether executeTransfer ever committed
// a transfer tagged with this reference, and if so, its transaction_id.
// found=false is a normal, meaningful answer (the reference was never
// used, or the transfer never actually executed) — not an error. Both
// entries of a call share the same reference, so LIMIT 1 is enough; there
// is no ambiguity to resolve between them.
func getTransactionByReference(ctx context.Context, pool *pgxpool.Pool, reference string) (transactionID string, found bool, err error) {
	err = pool.QueryRow(ctx,
		"SELECT transaction_id FROM entries WHERE reference = $1 LIMIT 1",
		reference,
	).Scan(&transactionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("look up entry by reference: %w", err)
	}
	return transactionID, true, nil
}

// applyBalanceDelta adds delta to ledgerAccountID's cached balance in
// account_balances within tx, creating the row (seeded at delta) if this is
// the account's first balance-affecting write. Safe to call without an
// extra lock because callers only ever invoke it for accounts they have
// already locked FOR UPDATE via lockLedgerAccount within the same tx, so no
// concurrent writer can be racing this update.
func applyBalanceDelta(ctx context.Context, tx pgx.Tx, ledgerAccountID string, delta int64) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO account_balances (ledger_account_id, balance, updated_at)
		 VALUES ($1, $2, now())
		 ON CONFLICT (ledger_account_id)
		 DO UPDATE SET balance = account_balances.balance + EXCLUDED.balance, updated_at = now()`,
		ledgerAccountID, delta,
	)
	if err != nil {
		return fmt.Errorf("update account_balances: %w", err)
	}
	return nil
}
