package migrations

import (
	"context"

	"github.com/uptrace/bun"
)

func init() {
	Migrations.MustRegister(addSAPRegistered, noopDown)
}

// addSAPRegistered records whether the SAP Service Layer entry was created.
// Rows with 0 are retried on a subsequent upload of the same content, even
// though the upload itself is deduplicated.
func addSAPRegistered(ctx context.Context, d *bun.DB) error {
	_, err := d.ExecContext(ctx, `
		ALTER TABLE attachments ADD COLUMN sap_registered BOOLEAN NOT NULL DEFAULT 0;
	`)
	return err
}
