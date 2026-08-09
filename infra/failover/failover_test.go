package failover

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"neobank/pkg/pgha"
	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

// This test kills a running container. That is the point of it — a
// failover setup that is never exercised is a failover setup that does
// not work, and the only way to find out which one you have is to pull
// the plug — but it is also why it is behind an explicit opt-in rather
// than the DATABASE_URL-presence check the rest of the repo's integration
// tests use. `go test ./...` with a dev stack up must not take the
// database down as a side effect.
//
//	FAILOVER_TEST=1 go test ./infra/failover/... -v -timeout 15m
const failoverEnvVar = "FAILOVER_TEST"

const (
	// Everything reaches the cluster through HAProxy, exactly as the
	// services do — the test is not allowed a privileged view of which
	// node is which, because neither is the application.
	defaultLeaderURL       = "postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable"
	defaultSyncStandbyURL  = "postgres://neobank:neobank_dev_password@localhost:5434/neobank?sslmode=disable"
	defaultLedgerAddr      = "localhost:8083"
	defaultTransfersHealth = "http://localhost:8084/healthz"

	// transferAmount is deliberately tiny: this test measures timing and
	// checks an invariant, and small amounts keep the funded account from
	// ever running dry no matter how long the write loop runs.
	transferAmount = 1

	// writeWorkers is "activity" during the kill. More than one because a
	// single sequential writer would not exercise the pool — the thing
	// that has to recover is a pool of connections, several of which are
	// checked out and mid-query at the moment the node dies.
	writeWorkers = 4

	// writeInterval paces each worker. ~80 writes/sec across four workers
	// is enough resolution to time the outage to well under a second
	// without the load itself becoming the variable under test.
	writeInterval = 50 * time.Millisecond

	// perCallTimeout bounds one ExecuteTransfer. Long enough that a
	// healthy call never trips it, short enough that a call issued into
	// the failover window fails and gets recorded rather than blocking a
	// worker for the whole outage.
	perCallTimeout = 3 * time.Second
)

func requireFailoverEnv(t *testing.T) {
	t.Helper()
	if os.Getenv(failoverEnvVar) == "" {
		t.Skipf("%s not set; skipping the test that kills a Postgres node (see this file's doc comment)", failoverEnvVar)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newPool(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	// pgha.NewPool, not pgxpool.New: the test's own pool should behave
	// like the services' pools during a failover, or it would be
	// measuring something the application does not experience.
	pool, err := pgha.NewPool(context.Background(), url)
	if err != nil {
		t.Fatalf("create pool for %s: %v", url, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newLedgerClient(t *testing.T) ledgerv1.LedgerServiceClient {
	t.Helper()
	addr := envOr("LEDGER_GRPC_ADDR", defaultLedgerAddr)
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("create ledger gRPC client for %s: %v", addr, err)
	}
	t.Cleanup(func() { conn.Close() })
	return ledgerv1.NewLedgerServiceClient(conn)
}

// randomUUID generates a v4 UUID without pulling in a dependency, the
// same way ledger_test.go and transfer_test.go already do.
func randomUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("generate uuid: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// testAccounts is a self-contained set of ledger accounts created fresh
// per test run. Self-contained matters for the invariant assertions:
// the dev database holds other tests' and the running services' entries,
// so a whole-table SUM would be correct in principle and useless in
// practice. Scoping to accounts this test created — including its own
// private "external world" counterparty rather than the shared sentinel
// devtopup uses — makes SUM(entries) = 0 an exact claim about exactly
// the money this test moved.
type testAccounts struct {
	fromAccountID    string
	toAccountID      string
	fromLedgerID     string
	toLedgerID       string
	externalLedgerID string
}

func (a testAccounts) ledgerIDs() []string {
	return []string{a.fromLedgerID, a.toLedgerID, a.externalLedgerID}
}

// setupAccounts creates two ledger accounts through the real (idempotent)
// gRPC API and funds one of them.
//
// The funding write is direct SQL rather than an RPC, for the reason
// devtopup documents: issuing money into the system is by definition the
// source account going negative, and ExecuteTransfer refuses exactly
// that. It is still a balanced pair sharing one transaction_id, so it
// upholds the invariant this test goes on to assert rather than quietly
// seeding a violation of it.
func setupAccounts(t *testing.T, ctx context.Context, client ledgerv1.LedgerServiceClient, pool *pgxpool.Pool, funding int64) testAccounts {
	t.Helper()

	accounts := testAccounts{
		fromAccountID: randomUUID(t),
		toAccountID:   randomUUID(t),
	}

	for _, id := range []string{accounts.fromAccountID, accounts.toAccountID} {
		if _, err := client.CreateLedgerAccount(ctx, &ledgerv1.CreateLedgerAccountRequest{AccountId: id}); err != nil {
			t.Fatalf("CreateLedgerAccount(%s): %v", id, err)
		}
	}

	accounts.fromLedgerID = ledgerAccountIDFor(t, ctx, pool, accounts.fromAccountID)
	accounts.toLedgerID = ledgerAccountIDFor(t, ctx, pool, accounts.toAccountID)
	accounts.externalLedgerID = ensureLedgerAccountRow(t, ctx, pool, randomUUID(t))

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin funding tx: %v", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`WITH txn AS (SELECT gen_random_uuid() AS id)
		 INSERT INTO entries (transaction_id, ledger_account_id, amount)
		 SELECT txn.id, $1::uuid, $2::bigint FROM txn
		 UNION ALL
		 SELECT txn.id, $3::uuid, $4::bigint FROM txn`,
		accounts.fromLedgerID, funding, accounts.externalLedgerID, -funding,
	); err != nil {
		t.Fatalf("insert funding entries: %v", err)
	}

	// ledger-svc's overdraft check reads SUM(entries), but GetBalance
	// reads the account_balances cache — recompute both accounts' cached
	// rows so the two agree from the start and a later mismatch means
	// something real.
	for _, ledgerID := range []string{accounts.fromLedgerID, accounts.externalLedgerID} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO account_balances (ledger_account_id, balance, updated_at)
			 SELECT $1, COALESCE(SUM(amount), 0)::bigint, now() FROM entries WHERE ledger_account_id = $1
			 ON CONFLICT (ledger_account_id)
			 DO UPDATE SET balance = EXCLUDED.balance, updated_at = now()`,
			ledgerID,
		); err != nil {
			t.Fatalf("recompute cached balance for %s: %v", ledgerID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit funding tx: %v", err)
	}

	return accounts
}

func ledgerAccountIDFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, "SELECT id FROM ledger_accounts WHERE account_id = $1", accountID).Scan(&id); err != nil {
		t.Fatalf("look up ledger_accounts for %s: %v", accountID, err)
	}
	return id
}

func ensureLedgerAccountRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accountID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO ledger_accounts (account_id) VALUES ($1)
		 ON CONFLICT (account_id) DO UPDATE SET account_id = EXCLUDED.account_id
		 RETURNING id`,
		accountID,
	).Scan(&id); err != nil {
		t.Fatalf("upsert ledger_accounts(%s): %v", accountID, err)
	}
	return id
}

// attempt is one ExecuteTransfer call and what came back. The recorded
// timestamp is when the call RETURNED, which is what the timing
// assertions want: the question is when the application regained the
// ability to complete a write, not when it started trying.
type attempt struct {
	at            time.Time
	transactionID string
	err           error
}

// recorder collects attempts from all writers. Nothing here calls
// t.Fatalf — these methods run on worker goroutines, where that is not
// allowed; failures are reported by the main goroutine after the fact.
type recorder struct {
	mu       sync.Mutex
	attempts []attempt
}

func (r *recorder) record(a attempt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts = append(r.attempts, a)
}

func (r *recorder) snapshot() []attempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]attempt, len(r.attempts))
	copy(out, r.attempts)
	return out
}

// acknowledged returns the transaction ids ledger-svc reported as
// committed. These, and only these, are what "no acknowledged
// transaction is lost" is a claim about: a call that errored or timed out
// may or may not have committed, and the application was never told it
// had, so its absence afterwards would not be data loss.
func (r *recorder) acknowledged() []string {
	var ids []string
	for _, a := range r.snapshot() {
		if a.err == nil && a.transactionID != "" {
			ids = append(ids, a.transactionID)
		}
	}
	return ids
}

// runWriters starts writeWorkers goroutines transferring money through
// ledger-svc until ctx is cancelled.
func runWriters(ctx context.Context, client ledgerv1.LedgerServiceClient, accounts testAccounts, rec *recorder) *sync.WaitGroup {
	var wg sync.WaitGroup
	for i := 0; i < writeWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(writeInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}

				callCtx, cancel := context.WithTimeout(context.Background(), perCallTimeout)
				resp, err := client.ExecuteTransfer(callCtx, &ledgerv1.ExecuteTransferRequest{
					FromAccountId: accounts.fromAccountID,
					ToAccountId:   accounts.toAccountID,
					Amount:        transferAmount,
				})
				cancel()

				rec.record(attempt{at: time.Now(), transactionID: resp.GetTransactionId(), err: err})
			}
		}()
	}
	return &wg
}

// outageWindow is the write outage as the application experienced it.
type outageWindow struct {
	firstFailure time.Time
	recovered    time.Time
}

func (w outageWindow) duration() time.Duration { return w.recovered.Sub(w.firstFailure) }

// waitForWriteOutageAndRecovery finds the boundaries of the write outage
// that follows the kill: the first ExecuteTransfer to fail, and the first
// one to succeed after that failure.
//
// Anchoring recovery to the first failure rather than to the kill instant
// is not a detail — the first version of this measured "first success
// after killAt" and reported an 11ms failover, which is nonsense. Calls
// issued a moment BEFORE the kill were still in flight when it landed,
// and some of them committed on the old leader and returned successfully
// a few milliseconds after it. Those successes are real, but they say
// nothing about recovery: they were served by the node that just died.
// The window that matters starts when writes actually began failing.
func waitForWriteOutageAndRecovery(t *testing.T, rec *recorder, killAt time.Time, timeout time.Duration) outageWindow {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var window outageWindow

	for {
		for _, a := range rec.snapshot() {
			if !a.at.After(killAt) {
				continue
			}
			if window.firstFailure.IsZero() {
				if a.err != nil {
					window.firstFailure = a.at
				}
				continue
			}
			if a.err == nil && a.at.After(window.firstFailure) {
				window.recovered = a.at
				return window
			}
		}
		if time.Now().After(deadline) {
			if window.firstFailure.IsZero() {
				t.Fatalf("no write ever failed after the leader was killed — the kill did not take effect, so nothing was measured; cluster: %s", clusterSummary(t))
			}
			t.Fatalf("writes did not recover within %s of failing; cluster: %s", timeout, clusterSummary(t))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestSyncStandbyHoldsAcknowledgedTransactionImmediately is the
// prerequisite for everything the failover test claims, checked
// separately so that if it breaks, the failure says so plainly instead of
// surfacing as a confusing "transaction lost" much later.
//
// It asserts the synchronous-replication guarantee with no retry and no
// sleep: the instant ledger-svc says a transfer committed, both of its
// entries are already readable on the synchronous standby. That is what
// synchronous_commit + Patroni's synchronous_mode were paid for in write
// latency, and it is precisely why promoting the sync standby cannot lose
// an acknowledged transaction.
func TestSyncStandbyHoldsAcknowledgedTransactionImmediately(t *testing.T) {
	requireFailoverEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := newLedgerClient(t)
	leaderPool := newPool(t, envOr("DATABASE_URL", defaultLeaderURL))
	syncPool := newPool(t, envOr("SYNC_STANDBY_URL", defaultSyncStandbyURL))

	// Confirm the standby really is one, so a misrouted port cannot make
	// this test pass by reading the leader.
	var inRecovery bool
	if err := syncPool.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery); err != nil {
		t.Fatalf("probe sync standby: %v", err)
	}
	if !inRecovery {
		t.Fatal("the node behind SYNC_STANDBY_URL is not in recovery — it is a leader, not a standby")
	}

	accounts := setupAccounts(t, ctx, client, leaderPool, 1_000_000)

	for i := 0; i < 20; i++ {
		resp, err := client.ExecuteTransfer(ctx, &ledgerv1.ExecuteTransferRequest{
			FromAccountId: accounts.fromAccountID,
			ToAccountId:   accounts.toAccountID,
			Amount:        transferAmount,
		})
		if err != nil {
			t.Fatalf("ExecuteTransfer #%d: %v", i, err)
		}

		// No sleep, no polling: if this needs a retry to pass, the
		// replication is not synchronous.
		var entries int
		if err := syncPool.QueryRow(ctx,
			"SELECT count(*) FROM entries WHERE transaction_id = $1", resp.GetTransactionId(),
		).Scan(&entries); err != nil {
			t.Fatalf("read transaction %s from sync standby: %v", resp.GetTransactionId(), err)
		}
		if entries != 2 {
			t.Fatalf("transaction %s has %d entries on the synchronous standby immediately after commit, want 2 — replication is not synchronous",
				resp.GetTransactionId(), entries)
		}
	}
}

// TestFailoverKillLeaderLosesNoAcknowledgedTransaction is the sprint's
// definition of done, executed:
//
//	docker kill the leader while transfers are in flight
//	  → Patroni elects a new leader
//	  → the application reconnects on its own, without being restarted
//	  → transfers pass again, and how long that took is recorded
//	  → every transaction acknowledged before the kill is on the new leader
//	  → SUM(entries) = 0 still holds
//	  → the old node comes back as a replica, not a second leader
func TestFailoverKillLeaderLosesNoAcknowledgedTransaction(t *testing.T) {
	requireFailoverEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client := newLedgerClient(t)
	leaderPool := newPool(t, envOr("DATABASE_URL", defaultLeaderURL))

	oldLeader, ok := leaderName(t)
	if !ok {
		t.Fatalf("no leader before the test even started; cluster: %s", clusterSummary(t))
	}
	t.Logf("cluster before kill: %s", clusterSummary(t))

	// The application containers must survive this untouched. Captured
	// before anything is killed so "it recovered" cannot secretly mean
	// "docker restarted it and the new process connected fine" — which
	// would prove nothing about the pools.
	appServices := []string{"ledger-svc", "transfers-svc", "notifications-svc"}
	before := make(map[string]containerIdentity, len(appServices))
	for _, svc := range appServices {
		before[svc] = inspectContainer(t, svc)
	}

	accounts := setupAccounts(t, ctx, client, leaderPool, 10_000_000)

	rec := &recorder{}
	writerCtx, stopWriters := context.WithCancel(ctx)
	wg := runWriters(writerCtx, client, accounts, rec)
	defer func() {
		stopWriters()
		wg.Wait()
	}()

	// Let the load reach steady state, and confirm it is actually
	// succeeding — killing a node while the writers were already failing
	// would measure nothing.
	time.Sleep(3 * time.Second)
	if len(rec.acknowledged()) == 0 {
		t.Fatalf("no successful transfers before the kill; is the stack up? cluster: %s", clusterSummary(t))
	}
	ackedBeforeKill := rec.acknowledged()
	t.Logf("steady state: %d transfers acknowledged before the kill", len(ackedBeforeKill))

	// SIGKILL, not a graceful stop. `docker compose stop` would let
	// Patroni shut down cleanly and hand the leader key over, which is
	// the easy case and not the one that matters — a leader that dies
	// abruptly leaves its key in etcd to expire on its own, and that
	// expiry is the failover budget being measured here.
	killAt := time.Now()
	if _, err := runDocker("kill", oldLeader); err != nil {
		t.Fatalf("kill leader %s: %v", oldLeader, err)
	}
	t.Logf("killed leader %s at %s", oldLeader, killAt.Format(time.RFC3339Nano))

	newLeader, electionTook := waitForLeader(t, oldLeader, 2*time.Minute)
	outage := waitForWriteOutageAndRecovery(t, rec, killAt, 3*time.Minute)

	// Keep writing briefly past recovery, so the post-failover assertions
	// cover transactions written by the NEW leader and not just the
	// backlog from the old one.
	time.Sleep(2 * time.Second)
	stopWriters()
	wg.Wait()

	t.Logf("FAILOVER TIMING")
	t.Logf("  leader %s killed at        %s", oldLeader, killAt.Format("15:04:05.000"))
	t.Logf("  writes began failing at    %s (+%s)", outage.firstFailure.Format("15:04:05.000"), outage.firstFailure.Sub(killAt).Truncate(time.Millisecond))
	t.Logf("  %s elected leader after    %s (polled, ~1s resolution)", newLeader, electionTook.Truncate(100*time.Millisecond))
	t.Logf("  writes succeeded again at  %s (+%s)", outage.recovered.Format("15:04:05.000"), outage.recovered.Sub(killAt).Truncate(time.Millisecond))
	t.Logf("  ==> WRITE OUTAGE: %s (kill to recovery: %s)", outage.duration().Truncate(time.Millisecond), outage.recovered.Sub(killAt).Truncate(time.Millisecond))
	t.Logf("cluster after failover: %s", clusterSummary(t))

	attempts := rec.snapshot()
	var failed int
	for _, a := range attempts {
		if a.err != nil {
			failed++
		}
	}
	t.Logf("write attempts: %d total, %d acknowledged, %d failed during the outage", len(attempts), len(rec.acknowledged()), failed)

	// ---- DoD 1: no acknowledged transaction was lost ----
	//
	// Checked against the NEW leader, through the same HAProxy address as
	// always — which now resolves to a different container than it did
	// when these transactions were acknowledged.
	assertTransactionsPresent(t, ctx, leaderPool, ackedBeforeKill, "acknowledged before the kill")
	assertTransactionsPresent(t, ctx, leaderPool, rec.acknowledged(), "acknowledged across the whole run")

	// ---- DoD 2: the ledger still balances ----
	assertLedgerBalanced(t, ctx, leaderPool, accounts)

	// ---- DoD 3: the application recovered on its own ----
	for _, svc := range appServices {
		after := inspectContainer(t, svc)
		if after != before[svc] {
			t.Errorf("%s was restarted during the failover (before: %+v, after: %+v) — its pool did not recover on its own, which is the thing this test exists to check",
				svc, before[svc], after)
		}
	}
	assertTransfersHealthy(t, 60*time.Second)

	// ---- DoD 4: the old leader rejoins as a replica, not a second one ----
	t.Logf("restarting %s", oldLeader)
	if _, err := runDocker("start", oldLeader); err != nil {
		t.Fatalf("restart old leader %s: %v", oldLeader, err)
	}
	rejoined := waitForMemberRole(t, oldLeader, 3*time.Minute, func(m member) bool {
		return !m.isLeader() && (m.State == "running" || m.State == "streaming")
	})
	t.Logf("%s rejoined as role=%s state=%s", rejoined.Name, rejoined.Role, rejoined.State)

	// leaderName fails the test outright on more than one leader, so
	// simply calling it here is the split-brain assertion.
	current, ok := leaderName(t)
	if !ok {
		t.Fatalf("no leader after the old node rejoined; cluster: %s", clusterSummary(t))
	}
	if current != newLeader {
		t.Errorf("leader changed again after %s rejoined: %s -> %s; a returning node must not take the leadership back on its own",
			oldLeader, newLeader, current)
	}
	t.Logf("cluster after rejoin: %s", clusterSummary(t))

	// The invariant one more time, now that all three nodes are back and
	// the rejoined one has replayed whatever it was missing.
	assertLedgerBalanced(t, ctx, leaderPool, accounts)
}

// onLeader runs a read against whoever is leader now, retrying if the
// read itself catches the tail of the failover.
//
// The verification queries need this for the same reason the services do:
// this test's own pool held connections to the node that was killed, and
// HAProxy tore them down. The first attempt after recovery can land on
// one of those and fail with "unexpected EOF" — which it did, failing an
// early run of this test at the verification step and reporting it as
// data loss when nothing had been lost at all. A test whose subject is
// transient connection failures must not itself fall over on one.
func onLeader(t *testing.T, ctx context.Context, pool *pgxpool.Pool, what string, fn func(context.Context) error) {
	t.Helper()
	if err := pgha.Retry(ctx, what, t.Logf, fn); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// assertTransactionsPresent checks that every acknowledged transaction is
// on the node currently serving writes, with BOTH of its entries. Both,
// not one: a single-sided entry would satisfy "the transaction is there"
// while being exactly the torn write that breaks double-entry.
func assertTransactionsPresent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, transactionIDs []string, label string) {
	t.Helper()

	if len(transactionIDs) == 0 {
		t.Fatalf("no transactions %s — nothing was actually verified", label)
	}

	type finding struct {
		id    string
		count int
	}
	var findings []finding

	onLeader(t, ctx, pool, "verify transactions "+label, func(attemptCtx context.Context) error {
		findings = nil
		rows, err := pool.Query(attemptCtx,
			`SELECT t.id, COALESCE(count(e.transaction_id), 0)
			   FROM unnest($1::uuid[]) AS t(id)
			   LEFT JOIN entries e ON e.transaction_id = t.id
			  GROUP BY t.id`,
			transactionIDs,
		)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var f finding
			if err := rows.Scan(&f.id, &f.count); err != nil {
				return err
			}
			if f.count != 2 {
				findings = append(findings, f)
			}
		}
		return rows.Err()
	})

	var missing, torn int
	for _, f := range findings {
		switch {
		case f.count == 0:
			missing++
			if missing <= 5 {
				t.Errorf("transaction %s was acknowledged but is NOT on the new leader — acknowledged data was lost", f.id)
			}
		default:
			torn++
			if torn <= 5 {
				t.Errorf("transaction %s has %d entries on the new leader, want 2 — a torn double-entry pair", f.id, f.count)
			}
		}
	}

	if missing > 0 || torn > 0 {
		t.Fatalf("%d of %d transactions %s are missing and %d are torn", missing, len(transactionIDs), label, torn)
	}
	t.Logf("verified all %d transactions %s are present and balanced on the current leader", len(transactionIDs), label)
}

// assertLedgerBalanced is the invariant, checked two ways.
//
// Scoped to this test's accounts for the reason ledger_test.go documents:
// the dev database holds other work's entries, so an unscoped sum would
// be fragile rather than strict. Within that scope both checks are exact.
func assertLedgerBalanced(t *testing.T, ctx context.Context, pool *pgxpool.Pool, accounts testAccounts) {
	t.Helper()

	ids := accounts.ledgerIDs()

	// 1. The global sum over everything this test touched.
	var total int64
	onLeader(t, ctx, pool, "compute SUM(entries)", func(attemptCtx context.Context) error {
		return pool.QueryRow(attemptCtx,
			"SELECT COALESCE(SUM(amount), 0)::bigint FROM entries WHERE ledger_account_id = ANY($1::uuid[])", ids,
		).Scan(&total)
	})
	if total != 0 {
		t.Errorf("SUM(entries) over this test's accounts = %d, want 0 — the ledger does not balance after failover", total)
	}

	// 2. Per transaction, which is strictly stronger: a pair of equal and
	// opposite errors in two different transactions would cancel out in
	// the global sum above and still be two broken transactions.
	type unbalancedTxn struct {
		id  string
		sum int64
	}
	var offenders []unbalancedTxn

	onLeader(t, ctx, pool, "check per-transaction balance", func(attemptCtx context.Context) error {
		offenders = nil
		rows, err := pool.Query(attemptCtx,
			`SELECT transaction_id, SUM(amount)::bigint
			   FROM entries
			  WHERE ledger_account_id = ANY($1::uuid[])
			  GROUP BY transaction_id
			 HAVING SUM(amount) <> 0`,
			ids,
		)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var u unbalancedTxn
			if err := rows.Scan(&u.id, &u.sum); err != nil {
				return err
			}
			offenders = append(offenders, u)
		}
		return rows.Err()
	})

	unbalanced := len(offenders)
	for i, u := range offenders {
		if i >= 5 {
			break
		}
		t.Errorf("transaction %s sums to %d, want 0", u.id, u.sum)
	}
	if unbalanced > 0 {
		t.Fatalf("%d transactions do not balance after failover", unbalanced)
	}

	t.Logf("invariant holds: SUM(entries) = 0 globally and per transaction")
}

// assertTransfersHealthy waits for transfers-svc's /healthz to report ok.
//
// That endpoint runs `SELECT 1` on the service's own write pool, so a 200
// is the service itself saying its pool reached the new leader. Combined
// with the container-identity check above — same container, never
// restarted — this is the "connection pools reconnect instead of staying
// dead" requirement, observed from outside the process.
func assertTransfersHealthy(t *testing.T, timeout time.Duration) {
	t.Helper()

	url := envOr("TRANSFERS_HEALTH_URL", defaultTransfersHealth)
	deadline := time.Now().Add(timeout)
	var lastErr error
	var lastStatus int

	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err != nil {
			lastErr = err
		} else {
			lastStatus = resp.StatusCode
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Logf("transfers-svc /healthz is 200 after the failover — its pool reconnected without a restart")
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Errorf("transfers-svc /healthz did not return 200 within %s (last status %d, last error %v) — its pool did not recover", timeout, lastStatus, lastErr)
}
