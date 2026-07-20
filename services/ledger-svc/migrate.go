package main

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// runMigrations applies any pending golang-migrate migrations embedded in
// the binary. It opens its own short-lived database/sql.DB via the pgx
// stdlib driver (registered by the blank import above), separate from the
// long-lived pgxpool.Pool main() opens afterward for request/consumer
// queries — golang-migrate's postgres driver operates on *sql.DB, not
// pgxpool.Pool, so the two connection types can't be shared.
//
// MigrationsTable is namespaced (not golang-migrate's bare default
// "schema_migrations") because every service in this repo shares one
// physical "neobank" Postgres database — the default name would collide
// if another service's migrator ever reads/writes the same table.
func runMigrations(databaseURL string) error {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open migration db: %w", err)
	}
	defer db.Close()

	driver, err := postgres.WithInstance(db, &postgres.Config{
		MigrationsTable: "schema_migrations_ledger_svc",
	})
	if err != nil {
		return fmt.Errorf("create postgres migration driver: %w", err)
	}

	source, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("load embedded migrations: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
