package migrations

import (
	"context"

	"github.com/uptrace/bun"
)

func init() {
	Migrations.MustRegister(addSAPAbsoluteEntry, noopDown)
}

// addSAPAbsoluteEntry stores the AbsoluteEntry id returned by SAP's
// Attachments2 call, so clients can reference the created SAP attachment.
func addSAPAbsoluteEntry(ctx context.Context, d *bun.DB) error {
	_, err := d.ExecContext(ctx, `
		ALTER TABLE attachments ADD COLUMN sap_absolute_entry BIGINT NOT NULL DEFAULT 0;
	`)
	return err
}
