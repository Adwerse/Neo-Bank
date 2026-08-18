package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"neobank/pkg/iban"
)

// backfillIBANs assigns an iban to every accounts row that predates the
// iban column (migration 000005 adds it nullable, precisely so existing
// rows can start out empty rather than blocking the migration itself),
// deriving it the exact same deterministic way createAccountForUser does
// for new accounts — one implementation of the mod-97-10 checksum math, in
// pkg/iban, not a second one reimplemented in SQL that could silently
// drift from it.
//
// Idempotent: only rows with iban IS NULL are selected, so every call
// after the first one that completes system-wide is a cheap zero-row
// no-op. Called synchronously from main() before any request-serving
// listener starts, so no HTTP/gRPC handler or Kafka consumer ever observes
// a row with a NULL iban.
func backfillIBANs(ctx context.Context, pool *pgxpool.Pool, bankCode string) error {
	rows, err := pool.Query(ctx, "SELECT id, account_number FROM accounts WHERE iban IS NULL")
	if err != nil {
		return fmt.Errorf("select accounts pending iban backfill: %w", err)
	}

	type pending struct {
		id, accountNumber string
	}
	var toBackfill []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.accountNumber); err != nil {
			rows.Close()
			return fmt.Errorf("scan account pending iban backfill: %w", err)
		}
		toBackfill = append(toBackfill, p)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate accounts pending iban backfill: %w", err)
	}
	rows.Close()

	for _, p := range toBackfill {
		sortCode, bbanAcctNum := ibanPartsFromAccountNumber(p.accountNumber)
		ibanValue, err := iban.Generate(bankCode, sortCode, bbanAcctNum)
		if err != nil {
			return fmt.Errorf("generate iban for account %s: %w", p.id, err)
		}
		if _, err := pool.Exec(ctx, "UPDATE accounts SET iban = $1 WHERE id = $2", ibanValue, p.id); err != nil {
			return fmt.Errorf("backfill iban for account %s: %w", p.id, err)
		}
	}
	return nil
}
