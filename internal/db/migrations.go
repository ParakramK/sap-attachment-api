package db

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"

	dbmigrations "github.com/parakram/sap-attachment-api/internal/db/migrations"
)

// runMigrations applies schema migrations.
func runMigrations(d *bun.DB) error {
	m := migrate.NewMigrator(d, dbmigrations.Migrations)
	ctx := context.Background()
	if err := m.Init(ctx); err != nil {
		return fmt.Errorf("init migration table: %w", err)
	}
	if _, err := m.Migrate(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
