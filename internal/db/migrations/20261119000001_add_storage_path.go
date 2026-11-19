package migrations

import (
	"context"

	"github.com/uptrace/bun"
)

func init() {
	Migrations.MustRegister(addStoragePath, noopDown)
}

// addStoragePath persists the consumer-facing full path alongside the
// backend-neutral object_path. Existing rows keep an empty value; the API
// derives their reference from object_path until they are re-uploaded.
func addStoragePath(ctx context.Context, d *bun.DB) error {
	_, err := d.ExecContext(ctx, `
		ALTER TABLE attachments ADD COLUMN storage_path VARCHAR(1024) NOT NULL DEFAULT '';
	`)
	return err
}
