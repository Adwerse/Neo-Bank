package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	accountsv1 "neobank/proto/gen/go/accounts/v1"
)

// historyEntry is one row of the unified operations feed GET /transfers
// returns (listTransfersHandler, http.go) — transfers, deposits, and
// withdrawals interleaved newest-first by (created_at, id), each tagged
// with Type so the frontend knows which fields apply and how to label it.
// Direction/CounterpartyAccountNumber are transfer-only (a deposit or
// withdrawal has exactly one account, no counterparty) and simply omitted
// for those types. The endpoint path and this response's own JSON key
// ("transfers", see listTransfersResponse) stay as-is despite now covering
// every operation type — renaming either is a bigger, purely cosmetic
// change (gateway route, frontend calls, OpenAPI operationId) for no
// functional gain.
type historyEntry struct {
	Type                      string    `json:"type"` // "transfer" | "deposit" | "withdrawal"
	ID                        string    `json:"id"`
	Amount                    int64     `json:"amount"`
	Status                    string    `json:"status"`
	FailureReason             *string   `json:"failure_reason,omitempty"`
	CreatedAt                 time.Time `json:"created_at"`
	UpdatedAt                 time.Time `json:"updated_at"`
	Direction                 string    `json:"direction,omitempty"`
	CounterpartyAccountNumber string    `json:"counterparty_account_number,omitempty"`
}

// isInvalidCursorErr reports whether err is Postgres rejecting a
// structurally-valid-looking cursor that decoded to a non-UUID string
// bound to a uuid column — see transfer.go's invalidTextRepresentation
// doc comment. getTransferHistoryPage has its own equivalent inline check
// (left as-is); this is shared by the two new per-type queries below.
func isInvalidCursorErr(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == invalidTextRepresentation
}

const depositHistoryQueryFirstPage = `
SELECT ` + depositColumns + `
FROM deposits
WHERE account_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2`

const depositHistoryQueryNextPage = `
SELECT ` + depositColumns + `
FROM deposits
WHERE account_id = $1 AND (created_at, id) < ($3, $4)
ORDER BY created_at DESC, id DESC
LIMIT $2`

// getDepositHistoryPage returns up to limit of accountID's deposits,
// newest first, plus whether more rows exist beyond this page — one of
// the three per-type sources getOperationHistoryPage merges into the
// unified feed. Simpler than getTransferHistoryPage: a deposit has exactly
// one owning account (no sender/recipient split), so this is a single
// indexed WHERE (idx_deposits_account_id, migration 000006) rather than a
// two-branch UNION ALL.
func getDepositHistoryPage(ctx context.Context, pool *pgxpool.Pool, accountID string, limit int32, cursor *pageCursor) ([]Deposit, bool, error) {
	fetchLimit := limit + 1

	var rows pgx.Rows
	var err error
	if cursor == nil {
		rows, err = pool.Query(ctx, depositHistoryQueryFirstPage, accountID, fetchLimit)
	} else {
		rows, err = pool.Query(ctx, depositHistoryQueryNextPage, accountID, fetchLimit, cursor.CreatedAt, cursor.ID)
	}
	if err != nil {
		if cursor != nil && isInvalidCursorErr(err) {
			return nil, false, errInvalidCursor
		}
		return nil, false, fmt.Errorf("query deposit history: %w", err)
	}
	defer rows.Close()

	deposits := make([]Deposit, 0, fetchLimit)
	for rows.Next() {
		var d Deposit
		if err := scanDeposit(rows, &d); err != nil {
			return nil, false, fmt.Errorf("scan deposit: %w", err)
		}
		deposits = append(deposits, d)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("query deposit history: %w", err)
	}

	hasMore := len(deposits) > int(limit)
	if hasMore {
		deposits = deposits[:limit]
	}
	return deposits, hasMore, nil
}

const withdrawalHistoryQueryFirstPage = `
SELECT ` + withdrawalColumns + `
FROM withdrawals
WHERE account_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2`

const withdrawalHistoryQueryNextPage = `
SELECT ` + withdrawalColumns + `
FROM withdrawals
WHERE account_id = $1 AND (created_at, id) < ($3, $4)
ORDER BY created_at DESC, id DESC
LIMIT $2`

// getWithdrawalHistoryPage mirrors getDepositHistoryPage exactly, against
// withdrawals (idx_withdrawals_account_id, migration 000009). Withdrawals
// have no frontend creation flow yet (simulation-only, see withdrawal.go),
// but any that exist — e.g. inserted directly for testing — must still
// render correctly in the unified feed, labeled as a simulation.
func getWithdrawalHistoryPage(ctx context.Context, pool *pgxpool.Pool, accountID string, limit int32, cursor *pageCursor) ([]Withdrawal, bool, error) {
	fetchLimit := limit + 1

	var rows pgx.Rows
	var err error
	if cursor == nil {
		rows, err = pool.Query(ctx, withdrawalHistoryQueryFirstPage, accountID, fetchLimit)
	} else {
		rows, err = pool.Query(ctx, withdrawalHistoryQueryNextPage, accountID, fetchLimit, cursor.CreatedAt, cursor.ID)
	}
	if err != nil {
		if cursor != nil && isInvalidCursorErr(err) {
			return nil, false, errInvalidCursor
		}
		return nil, false, fmt.Errorf("query withdrawal history: %w", err)
	}
	defer rows.Close()

	withdrawals := make([]Withdrawal, 0, fetchLimit)
	for rows.Next() {
		var w Withdrawal
		if err := scanWithdrawal(rows, &w); err != nil {
			return nil, false, fmt.Errorf("scan withdrawal: %w", err)
		}
		withdrawals = append(withdrawals, w)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("query withdrawal history: %w", err)
	}

	hasMore := len(withdrawals) > int(limit)
	if hasMore {
		withdrawals = withdrawals[:limit]
	}
	return withdrawals, hasMore, nil
}

// getOperationHistoryPage merges transfers, deposits, and withdrawals into
// one feed, newest first. Rather than a single heterogeneous SQL UNION
// (which would need NULL-padding across three mismatched column sets —
// sender/recipient ids only make sense for transfers, currency only for
// deposits, etc.), each type's own already-correct, already-sorted,
// already-(limit+1)-capped page — getTransferHistoryPage (transfer.go, a
// 2-way sender/recipient UNION ALL, unchanged) plus the two functions
// above — is fetched independently against the SAME (created_at, id)
// cursor, combined, re-sorted, and re-trimmed to limit.
//
// This is exact, not an approximation, for the same reason
// transferHistoryQueryFirstPage's own UNION ALL is exact (see that
// query's doc comment in transfer.go): the true top-limit of a merge of N
// independently sorted sources is always contained within the top-limit
// of EACH source alone, so fetching every source's own top-limit and
// re-merging can never miss a row that belongs on the final page.
//
// hasMore needs more than "did the combined pool exceed limit": even when
// the combined pool is exactly limit rows (nothing trimmed), an
// individual source can still report its OWN hasMore=true — meaning it
// has more rows beyond what it contributed here, that this merge never
// even fetched. Those rows necessarily sort after everything returned on
// this page (each source is itself sorted, and only its own top-limit was
// requested), so they belong on a subsequent page regardless of whether
// the pool itself overflowed. hasMore is therefore the OR of both checks,
// not just the pool-size one.
func getOperationHistoryPage(ctx context.Context, pool *pgxpool.Pool, accountsClient accountsv1.AccountsServiceClient, accountID string, limit int32, cursor *pageCursor) ([]historyEntry, bool, error) {
	transfers, transfersMore, err := getTransferHistoryPage(ctx, pool, accountID, limit, cursor)
	if err != nil {
		return nil, false, err
	}
	deposits, depositsMore, err := getDepositHistoryPage(ctx, pool, accountID, limit, cursor)
	if err != nil {
		return nil, false, err
	}
	withdrawals, withdrawalsMore, err := getWithdrawalHistoryPage(ctx, pool, accountID, limit, cursor)
	if err != nil {
		return nil, false, err
	}

	counterpartyNumbers, err := resolveCounterpartyAccountNumbers(ctx, accountsClient, accountID, transfers)
	if err != nil {
		return nil, false, err
	}

	merged := make([]historyEntry, 0, len(transfers)+len(deposits)+len(withdrawals))
	for _, t := range transfers {
		entry := historyEntry{
			Type: "transfer", ID: t.ID, Amount: t.Amount, Status: t.Status,
			FailureReason: t.FailureReason, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		}
		counterpartyID := t.RecipientAccountID
		if t.SenderAccountID == accountID {
			entry.Direction = "outgoing"
		} else {
			entry.Direction = "incoming"
			counterpartyID = t.SenderAccountID
		}
		entry.CounterpartyAccountNumber = counterpartyNumbers[counterpartyID]
		merged = append(merged, entry)
	}
	for _, d := range deposits {
		merged = append(merged, historyEntry{
			Type: "deposit", ID: d.ID, Amount: d.Amount, Status: d.Status,
			FailureReason: d.FailureReason, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
		})
	}
	for _, w := range withdrawals {
		merged = append(merged, historyEntry{
			Type: "withdrawal", ID: w.ID, Amount: w.Amount, Status: w.Status,
			FailureReason: w.FailureReason, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
		})
	}

	sort.Slice(merged, func(i, j int) bool {
		if !merged[i].CreatedAt.Equal(merged[j].CreatedAt) {
			return merged[i].CreatedAt.After(merged[j].CreatedAt)
		}
		return merged[i].ID > merged[j].ID
	})

	hasMore := len(merged) > int(limit) || transfersMore || depositsMore || withdrawalsMore
	if len(merged) > int(limit) {
		merged = merged[:limit]
	}
	return merged, hasMore, nil
}

// resolveCounterpartyAccountNumbers batch-resolves every unique
// counterparty across transfers into human-readable account numbers in
// one RPC call — unchanged in spirit from listTransfersHandler's prior
// inline version (see TestListTransfersHandler_BatchResolvesCounterpartiesOnce
// in transfer_test.go), just extracted so getOperationHistoryPage can call
// it directly. Deposits/withdrawals have no counterparty, so they never
// contribute to counterpartyIDSet.
func resolveCounterpartyAccountNumbers(ctx context.Context, accountsClient accountsv1.AccountsServiceClient, accountID string, transfers []Transfer) (map[string]string, error) {
	counterpartyIDSet := make(map[string]struct{}, len(transfers))
	for _, t := range transfers {
		if t.SenderAccountID == accountID {
			counterpartyIDSet[t.RecipientAccountID] = struct{}{}
		} else {
			counterpartyIDSet[t.SenderAccountID] = struct{}{}
		}
	}
	if len(counterpartyIDSet) == 0 {
		return map[string]string{}, nil
	}
	counterpartyIDs := make([]string, 0, len(counterpartyIDSet))
	for id := range counterpartyIDSet {
		counterpartyIDs = append(counterpartyIDs, id)
	}

	resolved, err := accountsClient.ResolveAccountsByIds(ctx, &accountsv1.ResolveAccountsByIdsRequest{AccountIds: counterpartyIDs})
	if err != nil {
		return nil, fmt.Errorf("resolve counterparty account numbers: %w", err)
	}
	accountNumbers := make(map[string]string, len(resolved.GetAccounts()))
	for _, acc := range resolved.GetAccounts() {
		accountNumbers[acc.GetAccountId()] = acc.GetAccountNumber()
	}
	return accountNumbers, nil
}
