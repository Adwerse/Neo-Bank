// Command devtopup is a LOCAL-DEVELOPMENT-ONLY tool for putting money on a
// user's account. Until Stripe lands (sprint 9) there is no real way for a
// user to receive funds, so an empty dashboard would have nothing to show;
// this fills that gap for local testing.
//
// It moves `amount` minor units (cents) from a well-known genesis account to
// the target account via ledger-svc's ordinary ExecuteTransfer RPC — the same
// real code path (row locks, overdraft check, balance-cache update) a
// production transfer would take, not a bespoke SQL insert.
//
// The one thing that genuinely cannot go through ExecuteTransfer is issuing
// new money into the system: ExecuteTransfer forbids the source account going
// negative, but issuance is by definition the source going negative. So when
// genesis doesn't hold enough to cover the top-up, this tool mints money into
// genesis directly in the database (a balanced pair against an "external
// world" account that absorbs the negative, keeping SUM(entries)=0), then does
// the real transfer. That direct mint is exactly why this is a dev tool and
// not a production HTTP endpoint.
//
// It assumes ledger-svc's migrations have already been applied (e.g. by
// starting the service once via docker compose) — it does not run migrations.
//
// Usage (from services/ledger-svc):
//
//	DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
//	LEDGER_GRPC_ADDR="localhost:8083" \
//	  go run ./cmd/devtopup --account-id <accounts.id> --amount 50000
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	ledgerv1 "neobank/proto/gen/go/ledger/v1"
)

const (
	// genesisAccountID matches cmd/seed's genesis account — the in-system
	// source funds flow out of.
	genesisAccountID = "00000000-0000-0000-0000-000000000001"
	// externalWorldAccountID is the counterparty that absorbs the negative
	// side of an issuance, representing money entering from outside the bank.
	// The all-zeros UUID is a deliberate "outside the system" sentinel,
	// distinct from genesis and cmd/seed's sample accounts.
	externalWorldAccountID = "00000000-0000-0000-0000-000000000000"

	// genesisMintChunk is how much to mint into genesis when it runs short —
	// far more than any single dev top-up needs, so minting is rare rather
	// than once per run.
	genesisMintChunk int64 = 1_000_000_000_000

	defaultLedgerAddr = "localhost:8083"
)

func main() {
	accountID := flag.String("account-id", "", "target account id (accounts.id) to top up")
	amount := flag.Int64("amount", 0, "amount to add, in minor units (cents)")
	flag.Parse()

	if *accountID == "" {
		log.Fatal("devtopup: --account-id is required (the accounts.id of the user's account)")
	}
	if *amount <= 0 {
		log.Fatalf("devtopup: --amount must be positive, got %d", *amount)
	}
	if *accountID == genesisAccountID || *accountID == externalWorldAccountID {
		log.Fatalf("devtopup: refusing to top up the genesis/external account (%s)", *accountID)
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("devtopup: DATABASE_URL is not set (e.g. postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable)")
	}
	ledgerAddr := os.Getenv("LEDGER_GRPC_ADDR")
	if ledgerAddr == "" {
		ledgerAddr = defaultLedgerAddr
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := grpc.NewClient(ledgerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("devtopup: failed to create ledger gRPC client for %s: %v", ledgerAddr, err)
	}
	defer conn.Close()
	client := ledgerv1.NewLedgerServiceClient(conn)

	// Ensure both ledger accounts exist, through the real (idempotent) API.
	// The target's ledger account normally already exists (created by
	// accounts-svc off UserActivated); this makes the tool robust if it
	// doesn't yet.
	if _, err := client.CreateLedgerAccount(ctx, &ledgerv1.CreateLedgerAccountRequest{AccountId: genesisAccountID}); err != nil {
		log.Fatalf("devtopup: ensure genesis ledger account: %v", err)
	}
	if _, err := client.CreateLedgerAccount(ctx, &ledgerv1.CreateLedgerAccountRequest{AccountId: *accountID}); err != nil {
		log.Fatalf("devtopup: ensure target ledger account %s: %v", *accountID, err)
	}

	dbConn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		log.Fatalf("devtopup: failed to connect to postgres: %v", err)
	}
	defer dbConn.Close(ctx)

	if err := ensureGenesisFunded(ctx, dbConn, *amount); err != nil {
		log.Fatalf("devtopup: %v", err)
	}

	// The actual top-up: an ordinary transfer through ledger-svc.
	transfer, err := client.ExecuteTransfer(ctx, &ledgerv1.ExecuteTransferRequest{
		FromAccountId: genesisAccountID,
		ToAccountId:   *accountID,
		Amount:        *amount,
	})
	if err != nil {
		log.Fatalf("devtopup: ExecuteTransfer(genesis -> %s, %d): %v", *accountID, *amount, err)
	}

	bal, err := client.GetBalance(ctx, &ledgerv1.GetBalanceRequest{AccountId: *accountID})
	if err != nil {
		log.Fatalf("devtopup: GetBalance(%s) after transfer: %v", *accountID, err)
	}

	log.Printf("devtopup: added %d to account %s (transaction %s); balance is now %d",
		*amount, *accountID, transfer.GetTransactionId(), bal.GetBalance())
}

// ensureGenesisFunded mints money into genesis if its spendable balance
// (SUM of its entries — the same quantity ExecuteTransfer's overdraft check
// reads) is below `amount`. Issuance can't go through ExecuteTransfer (it
// would be genesis going negative), so it's a direct, balanced DB write:
// external -> genesis, with both accounts' cached balances recomputed from
// the log so the cache stays consistent.
func ensureGenesisFunded(ctx context.Context, conn *pgx.Conn, amount int64) error {
	genesisLedgerID, err := ensureLedgerAccountRow(ctx, conn, genesisAccountID)
	if err != nil {
		return fmt.Errorf("ensure genesis ledger row: %w", err)
	}

	spendable, err := sumEntries(ctx, conn, genesisLedgerID)
	if err != nil {
		return fmt.Errorf("read genesis spendable balance: %w", err)
	}
	if spendable >= amount {
		return nil
	}

	externalLedgerID, err := ensureLedgerAccountRow(ctx, conn, externalWorldAccountID)
	if err != nil {
		return fmt.Errorf("ensure external ledger row: %w", err)
	}

	// Mint enough to cover this top-up, overshooting by at least a chunk so we
	// don't re-mint on every run. `amount - spendable` is what's missing;
	// spendable may be negative (e.g. after cmd/seed), so this brings genesis
	// comfortably positive.
	mint := amount - spendable
	if mint < genesisMintChunk {
		mint = genesisMintChunk
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin mint transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Balanced pair sharing one transaction_id: external is debited, genesis
	// credited. SUM(entries) across the two nets to zero, so the global
	// double-entry invariant holds.
	if _, err := tx.Exec(ctx,
		`WITH txn AS (SELECT gen_random_uuid() AS id)
		 INSERT INTO entries (transaction_id, ledger_account_id, amount)
		 SELECT txn.id, $1, $2 FROM txn
		 UNION ALL
		 SELECT txn.id, $3, $4 FROM txn`,
		genesisLedgerID, mint, externalLedgerID, -mint,
	); err != nil {
		return fmt.Errorf("insert mint entries: %w", err)
	}

	for _, ledgerAccountID := range []string{genesisLedgerID, externalLedgerID} {
		if err := recomputeCachedBalance(ctx, tx, ledgerAccountID); err != nil {
			return fmt.Errorf("recompute cached balance for %s: %w", ledgerAccountID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit mint transaction: %w", err)
	}
	log.Printf("devtopup: minted %d into genesis (was %d, needed %d)", mint, spendable, amount)
	return nil
}

// ensureLedgerAccountRow gets-or-creates the ledger_accounts row for accountID
// and returns its internal id, mirroring cmd/seed's ensureLedgerAccount (the
// ON CONFLICT ... DO UPDATE is a no-op write so RETURNING still yields the
// existing row's id on a repeat).
func ensureLedgerAccountRow(ctx context.Context, conn *pgx.Conn, accountID string) (string, error) {
	var ledgerAccountID string
	err := conn.QueryRow(ctx,
		`INSERT INTO ledger_accounts (account_id)
		 VALUES ($1)
		 ON CONFLICT (account_id) DO UPDATE SET account_id = EXCLUDED.account_id
		 RETURNING id`,
		accountID,
	).Scan(&ledgerAccountID)
	if err != nil {
		return "", fmt.Errorf("upsert ledger_accounts(account_id=%s): %w", accountID, err)
	}
	return ledgerAccountID, nil
}

// sumEntries returns SUM(amount) over a ledger account's entries — the
// spendable balance ExecuteTransfer checks, straight from the source-of-truth
// log rather than the cache.
func sumEntries(ctx context.Context, conn *pgx.Conn, ledgerAccountID string) (int64, error) {
	var sum int64
	err := conn.QueryRow(ctx,
		"SELECT COALESCE(SUM(amount), 0)::bigint FROM entries WHERE ledger_account_id = $1",
		ledgerAccountID,
	).Scan(&sum)
	return sum, err
}

// recomputeCachedBalance overwrites a ledger account's account_balances row
// with the value recomputed from its entries log, so the cache ExecuteTransfer
// and GetBalance read stays consistent with the entries the mint just wrote.
func recomputeCachedBalance(ctx context.Context, tx pgx.Tx, ledgerAccountID string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO account_balances (ledger_account_id, balance, updated_at)
		 SELECT $1, COALESCE(SUM(amount), 0)::bigint, now() FROM entries WHERE ledger_account_id = $1
		 ON CONFLICT (ledger_account_id)
		 DO UPDATE SET balance = EXCLUDED.balance, updated_at = now()`,
		ledgerAccountID,
	)
	return err
}
