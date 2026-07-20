// Command seed populates a local development database with sample ledger
// data: two ordinary ledger accounts, each funded via a genesis transaction
// from a special "system" ledger account. The system account's own balance
// goes deeply negative as a result — that's the expected representation of
// externally issued money in double-entry accounting, not a bug.
//
// It assumes ledger-svc's migrations have already been applied (e.g. by
// starting the service once via docker compose) — this tool does not run
// migrations itself.
//
// Usage (from services/ledger-svc):
//
//	DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
//	  go run ./cmd/seed
//
// Safe to re-run: every row is keyed off fixed UUID constants below, and
// each step is a no-op if its row(s) already exist.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5"
)

const (
	genesisAccountID = "00000000-0000-0000-0000-000000000001"
	sampleAccount1ID = "00000000-0000-0000-0000-000000000002"
	sampleAccount2ID = "00000000-0000-0000-0000-000000000003"

	genesisToAccount1TxnID = "00000000-0000-0000-0000-000000000101"
	genesisToAccount2TxnID = "00000000-0000-0000-0000-000000000102"

	account1StartingBalance int64 = 500000 // minor units, e.g. $5,000.00
	account2StartingBalance int64 = 250000 // minor units, e.g. $2,500.00
)

func main() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("seed: DATABASE_URL is not set (e.g. postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable)")
	}

	ctx := context.Background()

	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		log.Fatalf("seed: failed to connect to postgres: %v", err)
	}
	defer conn.Close(ctx)

	genesisRowID, err := ensureLedgerAccount(ctx, conn, genesisAccountID)
	if err != nil {
		log.Fatalf("seed: failed to ensure genesis ledger account: %v", err)
	}
	account1RowID, err := ensureLedgerAccount(ctx, conn, sampleAccount1ID)
	if err != nil {
		log.Fatalf("seed: failed to ensure sample ledger account 1: %v", err)
	}
	account2RowID, err := ensureLedgerAccount(ctx, conn, sampleAccount2ID)
	if err != nil {
		log.Fatalf("seed: failed to ensure sample ledger account 2: %v", err)
	}

	if err := seedTransaction(ctx, conn, genesisToAccount1TxnID, genesisRowID, account1RowID, account1StartingBalance); err != nil {
		log.Fatalf("seed: failed to seed genesis->account1 transaction: %v", err)
	}
	if err := seedTransaction(ctx, conn, genesisToAccount2TxnID, genesisRowID, account2RowID, account2StartingBalance); err != nil {
		log.Fatalf("seed: failed to seed genesis->account2 transaction: %v", err)
	}

	log.Println("seed: done")
}

// ensureLedgerAccount gets-or-creates the ledger_accounts row for accountID
// and returns its internal (id) primary key. The ON CONFLICT ... DO UPDATE
// is a no-op write (it rewrites account_id to itself) purely so RETURNING
// still yields the existing row's id on a repeat run — ON CONFLICT DO
// NOTHING would instead return zero rows when the account already exists.
func ensureLedgerAccount(ctx context.Context, conn *pgx.Conn, accountID string) (string, error) {
	const query = `
		INSERT INTO ledger_accounts (account_id)
		VALUES ($1)
		ON CONFLICT (account_id) DO UPDATE SET account_id = EXCLUDED.account_id
		RETURNING id`

	var ledgerAccountRowID string
	if err := conn.QueryRow(ctx, query, accountID).Scan(&ledgerAccountRowID); err != nil {
		return "", fmt.Errorf("upsert ledger_accounts(account_id=%s): %w", accountID, err)
	}
	return ledgerAccountRowID, nil
}

// seedTransaction writes the paired debit/credit entries for one logical
// transfer, unless entries for transactionID already exist (making re-runs
// a no-op). Both entries are inserted in a single database transaction so
// the sum-zero double-entry invariant is never observable in a
// half-written state.
func seedTransaction(ctx context.Context, conn *pgx.Conn, transactionID, fromLedgerAccountRowID, toLedgerAccountRowID string, amount int64) error {
	var alreadySeeded bool
	const existsQuery = `SELECT EXISTS(SELECT 1 FROM entries WHERE transaction_id = $1)`
	if err := conn.QueryRow(ctx, existsQuery, transactionID).Scan(&alreadySeeded); err != nil {
		return fmt.Errorf("check existing entries for transaction_id=%s: %w", transactionID, err)
	}
	if alreadySeeded {
		return nil
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once Commit below has succeeded

	const insertEntry = `
		INSERT INTO entries (transaction_id, ledger_account_id, amount)
		VALUES ($1, $2, $3)`

	if _, err := tx.Exec(ctx, insertEntry, transactionID, fromLedgerAccountRowID, -amount); err != nil {
		return fmt.Errorf("insert debit entry: %w", err)
	}
	if _, err := tx.Exec(ctx, insertEntry, transactionID, toLedgerAccountRowID, amount); err != nil {
		return fmt.Errorf("insert credit entry: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}
