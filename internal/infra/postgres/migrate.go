package postgres

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	migpg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for migrate's sql/postgres backend

	"github.com/Borislavv/polymarket-watchtower/db"
)

// Migrate applies every pending migration in `db/migrations`. The files
// are embedded via the `db` package's go:embed so the binary is self-
// contained at runtime — no external migration tool required.
//
// Idempotent: returns nil when there's nothing to apply
// (migrate.ErrNoChange is swallowed). Any real migration error is
// propagated and the caller should refuse to start.
func Migrate(dsn string) error {
	if dsn == "" {
		return fmt.Errorf("postgres migrate: empty DSN")
	}

	sub, err := iofs.New(db.Migrations, "migrations")
	if err != nil {
		return fmt.Errorf("postgres migrate: open embed fs: %w", err)
	}
	defer func() { _ = sub.Close() }()

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("postgres migrate: open sql conn: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	driver, err := migpg.WithInstance(sqlDB, &migpg.Config{})
	if err != nil {
		return fmt.Errorf("postgres migrate: driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", sub, "postgres", driver)
	if err != nil {
		return fmt.Errorf("postgres migrate: new: %w", err)
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("postgres migrate: up: %w", err)
	}
	return nil
}
