// Package migrations holds Bun schema migrations. Each migration lives in a
// file named <14-digit-timestamp>_<name>.go; Bun derives the migration name
// from the file name.
package migrations

import (
	"context"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
)

// Migrations is the shared registry wired into the migrator at startup.
var Migrations = migrate.NewMigrations()

func init() {
	Migrations.MustRegister(initAttachments, noopDown)
}

func initAttachments(ctx context.Context, d *bun.DB) error {
	// Column types mirror internal/db.Attachment bun tags. Timestamp-declared
	// columns are parsed back to time.Time by the SQLite driver.
	sql := `
		CREATE TABLE IF NOT EXISTS attachments (
			id           VARCHAR(64) PRIMARY KEY,
			filename     VARCHAR(512) NOT NULL,
			content_type VARCHAR(255) NOT NULL,
			size_bytes   BIGINT NOT NULL,
			sha256       VARCHAR(64) NOT NULL,
			status       VARCHAR(32) NOT NULL,
			backend      VARCHAR(32) NOT NULL,
			object_path  VARCHAR(512) NOT NULL,
			created_at   TIMESTAMPTZ NOT NULL,
			updated_at   TIMESTAMPTZ NOT NULL
		);
		-- One available attachment per content hash: makes content dedupe
		-- structurally race-safe. Staged (in-flight) rows are exempt.
		CREATE UNIQUE INDEX IF NOT EXISTS uniq_attachments_sha_available
			ON attachments (sha256) WHERE status = 'available';
		CREATE INDEX IF NOT EXISTS idx_attachments_status
			ON attachments (status);
	`
	_, err := d.ExecContext(ctx, sql)
	return err
}

func noopDown(context.Context, *bun.DB) error { return nil }
