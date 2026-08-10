package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// systemAccountIDs are the two accounts that are ALLOWED to hold a negative
// balance, and the only two. genesis goes negative by exactly the amount of
// money issued into the system, and external-world absorbs the other side
// of an issuance — that is what double-entry issuance looks like, not a
// broken invariant. Every other account going below zero is a bug, which is
// the whole point of the "no negative balances" check.
var systemAccountIDs = []string{genesisAccountID, externalWorldAccountID}

// checkResult is one invariant's verdict. Detail carries the offending rows
// when it fails, because "no negative balances: FAIL" without saying which
// account is a report you cannot act on.
type checkResult struct {
	Name    string   `json:"name"`
	What    string   `json:"what"`
	Passed  bool     `json:"passed"`
	Summary string   `json:"summary"`
	Detail  []string `json:"detail,omitempty"`
}

type verifyReport struct {
	Profile   string         `json:"profile"`
	RunPrefix string         `json:"run_prefix"`
	At        time.Time      `json:"at"`
	Cohort    int            `json:"cohort_accounts"`
	Checks    []checkResult  `json:"checks"`
	Stats     map[string]any `json:"stats"`
	Passed    bool           `json:"passed"`
}

func runVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	fixturesPath := fs.String("fixtures", "loadtest/fixtures/fixtures.json", "fixtures.json written by `lt setup`")
	databaseURL := fs.String("database-url", envOr("DATABASE_URL", defaultDatabaseURL), "Postgres URL of the current leader")
	profile := fs.String("profile", "", "label for this run, used in the report filename")
	out := fs.String("out", "", "write the report as JSON here (default loadtest/results/<profile>.verify.json)")
	settleWait := fs.Duration("settle-wait", 0, "wait this long before checking, letting in-flight settlement and the outbox relay finish")
	if err := fs.Parse(args); err != nil {
		return err
	}

	fixtures, err := readFixtures(*fixturesPath)
	if err != nil {
		return err
	}
	cohort := fixtures.AccountIDs()

	if *settleWait > 0 {
		// Not a fudge factor to make the numbers look better: a request
		// that k6 has already counted as finished can still have an
		// in-flight settlement behind it (settleTransfer's uncertain
		// branch leaves the row pending on purpose), and the outbox relay
		// polls on a one-second tick. Checking the instant k6 exits would
		// measure a system mid-flight and call it inconsistent. Anything
		// still unsettled after the wait is reported, not hidden.
		fmt.Printf("verify: waiting %s for in-flight work to settle\n", *settleWait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(*settleWait):
		}
	}

	conn, err := pgx.Connect(ctx, *databaseURL)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))

	report := verifyReport{
		Profile:   *profile,
		RunPrefix: fixtures.RunPrefix,
		At:        time.Now().UTC(),
		Cohort:    len(cohort),
		Passed:    true,
	}

	checks := []func(context.Context, *pgx.Conn, []string) (checkResult, error){
		checkEntriesSumZero,
		checkNoNegativeBalances,
		checkBalanceCacheMatchesLog,
		checkTransferEntriesPaired,
		checkNoEntriesForUnsettledTransfers,
		checkBalanceDeltaMatchesTransfers,
		checkCohortMoneyConserved,
		checkNoDuplicateIdempotencyKeys,
	}
	for _, check := range checks {
		result, err := check(ctx, conn, cohort)
		if err != nil {
			return err
		}
		report.Checks = append(report.Checks, result)
		if !result.Passed {
			report.Passed = false
		}
	}

	report.Stats, err = collectStats(ctx, conn, cohort)
	if err != nil {
		return err
	}

	printVerifyReport(report)

	path := *out
	if path == "" {
		name := *profile
		if name == "" {
			name = "run"
		}
		path = filepath.Join("loadtest", "results", name+".verify.json")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("verify: report written to %s\n", path)

	if !report.Passed {
		return errors.New("one or more invariants FAILED")
	}
	return nil
}

// checkEntriesSumZero is the double-entry invariant in its strongest form:
// not per transfer, not per account, but over every row in the table. Any
// non-zero result means money was created or destroyed somewhere, and no
// amount of throughput makes that acceptable.
//
// Deliberately NOT scoped to the cohort. The other checks narrow to the
// load-test accounts so unrelated dev data cannot produce a false failure;
// this one must not, because a bug that unbalances the books could just as
// easily land on genesis.
func checkEntriesSumZero(ctx context.Context, conn *pgx.Conn, _ []string) (checkResult, error) {
	var sum int64
	if err := conn.QueryRow(ctx, "SELECT COALESCE(SUM(amount), 0)::bigint FROM entries").Scan(&sum); err != nil {
		return checkResult{}, fmt.Errorf("sum entries: %w", err)
	}
	return checkResult{
		Name:    "entries_sum_zero",
		What:    "SUM(entries.amount) over the whole ledger is 0",
		Passed:  sum == 0,
		Summary: fmt.Sprintf("SUM(entries.amount) = %d", sum),
	}, nil
}

// checkNoNegativeBalances fails on a cohort account going below zero and
// only reports one outside the cohort.
//
// The asymmetry is deliberate and worth defending, because the tempting
// version of this check — "no account anywhere is negative" — is the one
// that gets switched off after a week. A dev stack accumulates accounts
// from earlier experiments (this one had three, left over from refund and
// failover testing before this directory existed), and a suite that fails
// on data it did not create teaches everyone to ignore its output.
//
// Scoping the failure to the cohort loses nothing: the load test only ever
// moves money between cohort accounts, so those are the only balances it
// can possibly drive negative. Everything outside is still counted and
// printed, clearly labelled as pre-existing, so it cannot be quietly
// forgotten either.
func checkNoNegativeBalances(ctx context.Context, conn *pgx.Conn, cohort []string) (checkResult, error) {
	rows, err := conn.Query(ctx,
		`SELECT la.account_id, ab.balance, la.account_id = ANY($2::uuid[]) AS in_cohort
		 FROM account_balances ab
		 JOIN ledger_accounts la ON la.id = ab.ledger_account_id
		 WHERE ab.balance < 0 AND la.account_id <> ALL($1::uuid[])
		 ORDER BY in_cohort DESC, ab.balance
		 LIMIT 20`,
		systemAccountIDs, cohort,
	)
	if err != nil {
		return checkResult{}, fmt.Errorf("query negative balances: %w", err)
	}
	defer rows.Close()

	var detail []string
	cohortNegatives := 0
	preexisting := 0
	for rows.Next() {
		var accountID string
		var balance int64
		var inCohort bool
		if err := rows.Scan(&accountID, &balance, &inCohort); err != nil {
			return checkResult{}, err
		}
		if inCohort {
			cohortNegatives++
			detail = append(detail, fmt.Sprintf("COHORT account %s balance %d", accountID, balance))
		} else {
			preexisting++
			detail = append(detail, fmt.Sprintf("pre-existing (not this run) account %s balance %d", accountID, balance))
		}
	}
	if err := rows.Err(); err != nil {
		return checkResult{}, err
	}
	return checkResult{
		Name:    "no_negative_balances",
		What:    "no cohort account holds a negative balance (genesis/external are negative by design)",
		Passed:  cohortNegatives == 0,
		Summary: fmt.Sprintf("%d cohort accounts below zero, %d pre-existing ones outside the cohort", cohortNegatives, preexisting),
		Detail:  detail,
	}, nil
}

// checkBalanceCacheMatchesLog compares account_balances against the entries
// log it is a projection of. This is not one of the four invariants the
// brief asks for, and it is here anyway because it is the one that the
// hot-account profile is most likely to break: account_balances is updated
// with a read-modify-write (applyBalanceDelta's `balance = balance +
// EXCLUDED.balance`) inside the same transaction that holds the row lock,
// so if the lock were ever taken on the wrong row — or not taken — the log
// would stay correct and the cache would silently drift. Every balance the
// API shows comes from the cache.
func checkBalanceCacheMatchesLog(ctx context.Context, conn *pgx.Conn, _ []string) (checkResult, error) {
	rows, err := conn.Query(ctx,
		`SELECT la.account_id, COALESCE(ab.balance, 0), COALESCE(e.total, 0)
		 FROM ledger_accounts la
		 LEFT JOIN account_balances ab ON ab.ledger_account_id = la.id
		 LEFT JOIN (SELECT ledger_account_id, SUM(amount)::bigint AS total FROM entries GROUP BY ledger_account_id) e
		        ON e.ledger_account_id = la.id
		 WHERE COALESCE(ab.balance, 0) <> COALESCE(e.total, 0)
		 LIMIT 20`,
	)
	if err != nil {
		return checkResult{}, fmt.Errorf("query balance drift: %w", err)
	}
	defer rows.Close()

	var detail []string
	for rows.Next() {
		var accountID string
		var cached, logged int64
		if err := rows.Scan(&accountID, &cached, &logged); err != nil {
			return checkResult{}, err
		}
		detail = append(detail, fmt.Sprintf("account %s: cache %d, log %d (drift %d)", accountID, cached, logged, cached-logged))
	}
	if err := rows.Err(); err != nil {
		return checkResult{}, err
	}
	return checkResult{
		Name:    "balance_cache_matches_log",
		What:    "account_balances equals SUM(entries) for every account",
		Passed:  len(detail) == 0,
		Summary: fmt.Sprintf("%d accounts where the cached balance disagrees with the log", len(detail)),
		Detail:  detail,
	}, nil
}

// checkTransferEntriesPaired is the duplicate-protection invariant, and it
// is worth being precise about why.
//
// "No duplicate idempotency keys" is enforced by a UNIQUE constraint, so
// asserting it proves almost nothing. The failure the duplicates profile
// actually hunts for is different and much worse: two concurrent requests
// carrying the same key both reaching ledger-svc, producing ONE transfers
// row and FOUR entries — the books balance, no constraint is violated, and
// the money moved twice. That shows up here as count(entries) = 4 for a
// transfer, which is exactly what this check rejects.
//
// It also pins direction and magnitude in the same pass: the sender's side
// must be exactly -amount and the recipient's exactly +amount, so a
// transfer that posted backwards or posted the wrong sum fails too.
func checkTransferEntriesPaired(ctx context.Context, conn *pgx.Conn, cohort []string) (checkResult, error) {
	rows, err := conn.Query(ctx,
		`WITH t AS (
		     SELECT id, sender_account_id, recipient_account_id, amount
		     FROM transfers
		     WHERE status = 'completed' AND sender_account_id = ANY($1::uuid[])
		 )
		 SELECT t.id,
		        count(e.id),
		        COALESCE(SUM(e.amount), 0),
		        COALESCE(SUM(CASE WHEN la.account_id = t.sender_account_id    THEN e.amount END), 0),
		        COALESCE(SUM(CASE WHEN la.account_id = t.recipient_account_id THEN e.amount END), 0),
		        t.amount
		 FROM t
		 LEFT JOIN entries e         ON e.reference = t.id
		 LEFT JOIN ledger_accounts la ON la.id = e.ledger_account_id
		 GROUP BY t.id, t.amount, t.sender_account_id, t.recipient_account_id
		 HAVING count(e.id) <> 2
		     OR COALESCE(SUM(e.amount), 0) <> 0
		     OR COALESCE(SUM(CASE WHEN la.account_id = t.sender_account_id    THEN e.amount END), 0) <> -t.amount
		     OR COALESCE(SUM(CASE WHEN la.account_id = t.recipient_account_id THEN e.amount END), 0) <>  t.amount
		 LIMIT 20`,
		cohort,
	)
	if err != nil {
		return checkResult{}, fmt.Errorf("query transfer/entry pairing: %w", err)
	}
	defer rows.Close()

	var detail []string
	for rows.Next() {
		var id string
		var count, net, senderSide, recipientSide, amount int64
		if err := rows.Scan(&id, &count, &net, &senderSide, &recipientSide, &amount); err != nil {
			return checkResult{}, err
		}
		detail = append(detail, fmt.Sprintf(
			"transfer %s (amount %d): %d entries, net %d, sender side %d, recipient side %d",
			id, amount, count, net, senderSide, recipientSide))
	}
	if err := rows.Err(); err != nil {
		return checkResult{}, err
	}

	var completed int64
	if err := conn.QueryRow(ctx,
		"SELECT count(*) FROM transfers WHERE status = 'completed' AND sender_account_id = ANY($1::uuid[])",
		cohort,
	).Scan(&completed); err != nil {
		return checkResult{}, err
	}
	return checkResult{
		Name:    "transfer_entries_paired",
		What:    "every completed transfer posted exactly one balanced debit/credit pair, in the right direction, for the right amount",
		Passed:  len(detail) == 0,
		Summary: fmt.Sprintf("%d completed transfers checked, %d malformed (a double-post would appear as 4 entries)", completed, len(detail)),
		Detail:  detail,
	}, nil
}

// checkNoEntriesForUnsettledTransfers catches the opposite error to a
// double-post: money that moved for a transfer the service recorded as
// failed or rejected. transfers-svc only ever writes 'failed' after
// ledger-svc returned a definite business error (settleTransfer), and
// 'rejected' before ledger-svc is called at all, so there must be no
// entries tagged with either.
//
// 'pending' is excluded here and reported in the stats instead: a pending
// transfer with entries is the legitimate, expected outcome of the
// settlement-uncertain path — the ledger committed, the response never
// arrived — and the reconciliation worker is what resolves it.
func checkNoEntriesForUnsettledTransfers(ctx context.Context, conn *pgx.Conn, cohort []string) (checkResult, error) {
	rows, err := conn.Query(ctx,
		`SELECT t.id, t.status, count(e.id), COALESCE(SUM(e.amount), 0)
		 FROM transfers t
		 JOIN entries e ON e.reference = t.id
		 WHERE t.sender_account_id = ANY($1::uuid[]) AND t.status IN ('failed', 'rejected')
		 GROUP BY t.id, t.status
		 LIMIT 20`,
		cohort,
	)
	if err != nil {
		return checkResult{}, fmt.Errorf("query entries for unsettled transfers: %w", err)
	}
	defer rows.Close()

	var detail []string
	for rows.Next() {
		var id, status string
		var count, net int64
		if err := rows.Scan(&id, &status, &count, &net); err != nil {
			return checkResult{}, err
		}
		detail = append(detail, fmt.Sprintf("transfer %s is %s but has %d entries (net %d)", id, status, count, net))
	}
	if err := rows.Err(); err != nil {
		return checkResult{}, err
	}
	return checkResult{
		Name:    "no_entries_for_failed_or_rejected",
		What:    "no money moved for a transfer recorded as failed or rejected",
		Passed:  len(detail) == 0,
		Summary: fmt.Sprintf("%d failed/rejected transfers have ledger entries", len(detail)),
		Detail:  detail,
	}, nil
}

// checkBalanceDeltaMatchesTransfers is the brief's "the number of executed
// transfers matches the sum of balance changes", stated per account and
// exactly.
//
// For each cohort account it compares two independently-derived numbers:
// what the transfers table says should have happened to this account
// (received minus sent, over completed transfers), and what the ledger
// actually did to it (the sum of entries tagged with those transfers' ids).
// The two come from different tables written by different services, so
// agreement is a real cross-check rather than a restatement — the transfers
// row is transfers-svc's record, the entries are ledger-svc's.
func checkBalanceDeltaMatchesTransfers(ctx context.Context, conn *pgx.Conn, cohort []string) (checkResult, error) {
	rows, err := conn.Query(ctx,
		`WITH cohort AS (SELECT unnest($1::uuid[]) AS account_id),
		 done AS (
		     SELECT id, sender_account_id, recipient_account_id, amount
		     FROM transfers
		     WHERE status = 'completed' AND sender_account_id = ANY($1::uuid[])
		 ),
		 expected AS (
		     SELECT c.account_id,
		            COALESCE((SELECT SUM(amount) FROM done WHERE recipient_account_id = c.account_id), 0)
		          - COALESCE((SELECT SUM(amount) FROM done WHERE sender_account_id    = c.account_id), 0) AS delta
		     FROM cohort c
		 ),
		 actual AS (
		     SELECT c.account_id,
		            COALESCE((SELECT SUM(e.amount)
		                      FROM entries e
		                      JOIN ledger_accounts la ON la.id = e.ledger_account_id
		                      WHERE la.account_id = c.account_id
		                        AND e.reference IN (SELECT id FROM done)), 0) AS delta
		     FROM cohort c
		 )
		 SELECT expected.account_id, expected.delta, actual.delta
		 FROM expected JOIN actual USING (account_id)
		 WHERE expected.delta <> actual.delta
		 LIMIT 20`,
		cohort,
	)
	if err != nil {
		return checkResult{}, fmt.Errorf("query balance delta reconciliation: %w", err)
	}
	defer rows.Close()

	var detail []string
	for rows.Next() {
		var accountID string
		var expected, actual int64
		if err := rows.Scan(&accountID, &expected, &actual); err != nil {
			return checkResult{}, err
		}
		detail = append(detail, fmt.Sprintf("account %s: transfers say %+d, ledger did %+d", accountID, expected, actual))
	}
	if err := rows.Err(); err != nil {
		return checkResult{}, err
	}
	return checkResult{
		Name:    "balance_delta_matches_transfers",
		What:    "per account, the net of completed transfers equals the net the ledger actually posted",
		Passed:  len(detail) == 0,
		Summary: fmt.Sprintf("%d of %d cohort accounts disagree", len(detail), len(cohort)),
		Detail:  detail,
	}, nil
}

// checkCohortMoneyConserved is the same conservation law seen from one step
// back, and it is the check that is hardest to satisfy by accident.
//
// Every transfer the load test makes is cohort account -> cohort account,
// so no matter how many of them ran, how they interleaved, or how many were
// retried, the total money held by the cohort must be exactly what setup
// funded into it. Money entered the cohort only through funding entries
// (which carry no reference — see fundAccounts) and moved only through
// transfer entries (which all do). If the total moved even by one minor
// unit, the run leaked or invented money somewhere the per-transfer checks
// did not look.
func checkCohortMoneyConserved(ctx context.Context, conn *pgx.Conn, cohort []string) (checkResult, error) {
	var totalBalance, totalFunded int64
	err := conn.QueryRow(ctx,
		`SELECT
		   COALESCE(SUM(e.amount), 0)::bigint,
		   COALESCE(SUM(e.amount) FILTER (WHERE e.reference IS NULL), 0)::bigint
		 FROM entries e
		 JOIN ledger_accounts la ON la.id = e.ledger_account_id
		 WHERE la.account_id = ANY($1::uuid[])`,
		cohort,
	).Scan(&totalBalance, &totalFunded)
	if err != nil {
		return checkResult{}, fmt.Errorf("query cohort conservation: %w", err)
	}
	return checkResult{
		Name:    "cohort_money_conserved",
		What:    "the cohort's total balance still equals what setup funded into it",
		Passed:  totalBalance == totalFunded,
		Summary: fmt.Sprintf("cohort holds %d, was funded %d (difference %d)", totalBalance, totalFunded, totalBalance-totalFunded),
	}, nil
}

// checkNoDuplicateIdempotencyKeys is the brief's fourth invariant taken
// literally. It is nearly free and nearly tautological — transfers has a
// UNIQUE constraint on idempotency_key, so this can only fail if that
// constraint were dropped — and it is kept for exactly that reason: it is
// the assertion that would notice a migration silently removing the
// protection everything else in the duplicates profile relies on.
// checkTransferEntriesPaired is where the interesting half of duplicate
// protection is actually tested.
func checkNoDuplicateIdempotencyKeys(ctx context.Context, conn *pgx.Conn, cohort []string) (checkResult, error) {
	var total, distinct int64
	err := conn.QueryRow(ctx,
		`SELECT count(*), count(DISTINCT idempotency_key)
		 FROM transfers WHERE sender_account_id = ANY($1::uuid[])`,
		cohort,
	).Scan(&total, &distinct)
	if err != nil {
		return checkResult{}, fmt.Errorf("query idempotency keys: %w", err)
	}
	return checkResult{
		Name:    "no_duplicate_idempotency_keys",
		What:    "one transfers row per idempotency key",
		Passed:  total == distinct,
		Summary: fmt.Sprintf("%d transfers, %d distinct keys", total, distinct),
	}, nil
}

// collectStats is reporting, not checking: numbers that describe the run
// rather than pass or fail it. transfers_pending and outbox_backlog are the
// two worth reading every time — a pending count that does not fall to zero
// means the reconciliation worker still has work, and a backlog that does
// not drain means the relay never caught up with what the run wrote.
func collectStats(ctx context.Context, conn *pgx.Conn, cohort []string) (map[string]any, error) {
	stats := map[string]any{}

	rows, err := conn.Query(ctx,
		`SELECT status, count(*), COALESCE(SUM(amount), 0)::bigint
		 FROM transfers WHERE sender_account_id = ANY($1::uuid[])
		 GROUP BY status ORDER BY status`,
		cohort,
	)
	if err != nil {
		return nil, fmt.Errorf("query transfer statuses: %w", err)
	}
	byStatus := map[string]map[string]int64{}
	for rows.Next() {
		var status string
		var count, sum int64
		if err := rows.Scan(&status, &count, &sum); err != nil {
			rows.Close()
			return nil, err
		}
		byStatus[status] = map[string]int64{"count": count, "amount": sum}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	stats["transfers_by_status"] = byStatus

	reasonRows, err := conn.Query(ctx,
		`SELECT COALESCE(failure_reason, '(none)'), count(*)
		 FROM transfers
		 WHERE sender_account_id = ANY($1::uuid[]) AND status IN ('failed', 'rejected')
		 GROUP BY 1 ORDER BY 2 DESC`,
		cohort,
	)
	if err != nil {
		return nil, fmt.Errorf("query failure reasons: %w", err)
	}
	reasons := map[string]int64{}
	for reasonRows.Next() {
		var reason string
		var count int64
		if err := reasonRows.Scan(&reason, &count); err != nil {
			reasonRows.Close()
			return nil, err
		}
		reasons[reason] = count
	}
	reasonRows.Close()
	if err := reasonRows.Err(); err != nil {
		return nil, err
	}
	stats["failure_reasons"] = reasons

	var entriesTotal, outboxBacklog, outboxTotal, fraudChecks int64
	if err := conn.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM entries),
		       (SELECT count(*) FROM outbox WHERE published_at IS NULL),
		       (SELECT count(*) FROM outbox),
		       (SELECT count(*) FROM fraud_checks WHERE account_id = ANY($1::uuid[]))`,
		cohort,
	).Scan(&entriesTotal, &outboxBacklog, &outboxTotal, &fraudChecks); err != nil {
		return nil, fmt.Errorf("query totals: %w", err)
	}
	stats["entries_total"] = entriesTotal
	stats["outbox_unpublished"] = outboxBacklog
	stats["outbox_total"] = outboxTotal
	stats["fraud_checks_for_cohort"] = fraudChecks

	return stats, nil
}

func printVerifyReport(r verifyReport) {
	fmt.Printf("\ninvariants after %s (cohort: %d accounts)\n\n", displayProfile(r.Profile), r.Cohort)
	for _, c := range r.Checks {
		mark := "PASS"
		if !c.Passed {
			mark = "FAIL"
		}
		fmt.Printf("  [%s] %-36s %s\n", mark, c.Name, c.Summary)
		fmt.Printf("         %s\n", c.What)
		for _, d := range c.Detail {
			fmt.Printf("         - %s\n", d)
		}
	}

	fmt.Printf("\n  stats\n")
	if byStatus, ok := r.Stats["transfers_by_status"].(map[string]map[string]int64); ok {
		for _, status := range []string{"completed", "failed", "rejected", "pending"} {
			if s, ok := byStatus[status]; ok {
				fmt.Printf("    transfers %-10s %8d  (%d minor units)\n", status, s["count"], s["amount"])
			}
		}
	}
	if reasons, ok := r.Stats["failure_reasons"].(map[string]int64); ok && len(reasons) > 0 {
		for reason, count := range reasons {
			fmt.Printf("    reason %-14s %8d\n", reason, count)
		}
	}
	for _, key := range []string{"entries_total", "outbox_unpublished", "outbox_total", "fraud_checks_for_cohort"} {
		if v, ok := r.Stats[key]; ok {
			fmt.Printf("    %-24s %8v\n", key, v)
		}
	}

	verdict := "ALL INVARIANTS HOLD"
	if !r.Passed {
		verdict = "INVARIANTS VIOLATED"
	}
	fmt.Printf("\n  %s\n\n", verdict)
}

func displayProfile(profile string) string {
	if strings.TrimSpace(profile) == "" {
		return "run"
	}
	return profile
}
