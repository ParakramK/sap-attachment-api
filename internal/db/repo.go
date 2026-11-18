package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/google/uuid"
)

// Attachment statuses.
const (
	StatusStaged    = "staged"    // row created, object not yet committed
	StatusAvailable = "available" // object committed, ready for consumers
)

// Attachment is one stored file. Content dedupe means at most one available
// row per sha256 (enforced by a partial unique index).
type Attachment struct {
	bun.BaseModel `bun:"table:attachments,alias:att"`

	ID          string    `bun:"id,pk"`
	Filename    string    `bun:"filename,notnull"`
	ContentType string    `bun:"content_type,notnull"`
	SizeBytes   int64     `bun:"size_bytes,notnull"`
	SHA256      string    `bun:"sha256,notnull"`
	Status      string    `bun:"status,notnull"`
	Backend     string    `bun:"backend,notnull"` // storage backend holding the object
	ObjectPath  string    `bun:"object_path,notnull"`
	CreatedAt   time.Time `bun:"created_at,notnull"`
	UpdatedAt   time.Time `bun:"updated_at,notnull"`
}

func NewAttachment() *Attachment {
	return &Attachment{ID: uuid.NewString()}
}

// Repo is the attachment store over Bun. Methods wrap driver errors in
// service-level sentinels where the caller needs to branch.
type Repo struct {
	d *bun.DB
}

func NewRepo(d *bun.DB) *Repo { return &Repo{d: d} }

// ErrContentExists signals a concurrent upload committed the same content
// first; callers should re-read by hash and return the existing row.
var ErrContentExists = errors.New("content already stored")

// Create inserts a new row. Content-hash collisions with an available row map
// to ErrContentExists.
func (r *Repo) Create(ctx context.Context, a *Attachment) error {
	_, err := r.d.NewInsert().Model(a).Exec(ctx)
	if isUniqueViolation(err) {
		return ErrContentExists
	}
	return err
}

// AvailableBySHA returns the available attachment with this content hash, or
// (nil, nil) when none exists.
func (r *Repo) AvailableBySHA(ctx context.Context, sha string) (*Attachment, error) {
	var a Attachment
	err := r.d.NewSelect().Model(&a).
		Where("sha256 = ? AND status = ?", sha, StatusAvailable).
		Limit(1).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ByID returns an attachment by ID, or (nil, nil) when not found.
func (r *Repo) ByID(ctx context.Context, id string) (*Attachment, error) {
	var a Attachment
	err := r.d.NewSelect().Model(&a).Where("id = ?", id).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// MarkAvailable flips a staged row to available after the object is committed.
func (r *Repo) MarkAvailable(ctx context.Context, id string) error {
	_, err := r.d.NewUpdate().
		Model((*Attachment)(nil)).
		Set("status = ?", StatusAvailable).
		Set("updated_at = ?", time.Now()).
		Where("id = ?", id).
		Exec(ctx)
	return err
}

// Delete removes a row entirely (hard delete, explicit lifecycle).
func (r *Repo) Delete(ctx context.Context, id string) error {
	_, err := r.d.NewDelete().Model((*Attachment)(nil)).Where("id = ?", id).Exec(ctx)
	return err
}

// StagedOlderThan returns in-flight rows whose object was never committed.
func (r *Repo) StagedOlderThan(ctx context.Context, cutoff time.Time) ([]*Attachment, error) {
	var rows []*Attachment
	err := r.d.NewSelect().Model(&rows).
		Where("status = ? AND created_at < ?", StatusStaged, cutoff).
		Scan(ctx)
	return rows, err
}

// isUniqueViolation detects the SQLite UNIQUE constraint error text. The
// modernc driver has no stable exported sentinel for it.
// shortcut: string match on driver error, revisit if a sentinel is added.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
