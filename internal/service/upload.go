package service

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/parakram/sap-attachment-api/internal/db"
	"github.com/parakram/sap-attachment-api/internal/storage"
)

// Uploader ingests content from any source (direct upload body, HTTP URL,
// S3 URI): it sniffs the content type, stages to disk with a SHA-256 digest,
// dedupes by content, commits atomically and maintains the DB lifecycle.
type Uploader struct {
	repo       *db.Repo
	store      storage.Storage
	stagingDir string
	maxBytes   int64
	now        func() time.Time
}

func NewUploader(repo *db.Repo, store storage.Storage, stagingDir string, maxBytes int64) (*Uploader, error) {
	if err := os.MkdirAll(stagingDir, 0o750); err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	return &Uploader{repo: repo, store: store, stagingDir: stagingDir, maxBytes: maxBytes, now: time.Now}, nil
}

// Input is one ingest source. Reader must yield the raw file content;
// Filename may be empty. Size is the source-declared size or -1 when unknown
// (informational only; the staged size is authoritative). IdempotencyKey, when
// set, becomes the stored file name (plus the original extension) so paths
// stay short and Windows-safe.
type Input struct {
	Reader         io.Reader
	Filename       string
	Size           int64
	IdempotencyKey string
}

// Upload stores the content. Dedupe: when this content hash is already
// available, the existing attachment is returned with deduped=true and the
// incoming bytes are discarded.
func (u *Uploader) Upload(ctx context.Context, in Input) (att *db.Attachment, deduped bool, err error) {
	// Remote sources hand us a body that must be closed after streaming.
	if closer, ok := in.Reader.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}

	// Sniff the real content type from the first bytes; caller-supplied types
	// are not trusted at this trust boundary.
	br := bufio.NewReader(in.Reader)
	head, _ := br.Peek(512)
	contentType := http.DetectContentType(head)

	staged, err := storage.Stage(u.stagingDir, br, u.maxBytes)
	if err != nil {
		return nil, false, err
	}
	discard := func() {
		_ = staged.Discard()
	}

	// Content dedupe: one available attachment per hash.
	existing, err := u.repo.AvailableBySHA(ctx, staged.SHA256)
	if err != nil {
		discard()
		return nil, false, fmt.Errorf("dedupe lookup: %w", err)
	}
	if existing != nil {
		discard()
		return existing, true, nil
	}

	a := db.NewAttachment()
	// The client's filename is never used as the stored name: it can be long
	// enough to break Windows paths. The UUID key (plus the original file's
	// short extension) is bounded and unique per request.
	name := sanitizeFilename(in.Filename)
	if in.IdempotencyKey != "" {
		name = sanitizeFilename(in.IdempotencyKey + storage.Ext(in.Filename))
	}
	a.Filename = name
	a.ContentType = contentType
	a.SizeBytes = staged.Size
	a.SHA256 = staged.SHA256
	a.Status = db.StatusStaged
	a.Backend = u.store.Name()
	a.ObjectPath = storage.ObjectPath(staged.SHA256, a.Filename)
	a.CreatedAt = u.now()
	a.UpdatedAt = a.CreatedAt

	// Row first (status=staged): a crash after commit is reconciled by the
	// sweeper, never silently lost.
	if err := u.repo.Create(ctx, a); err != nil {
		discard()
		if errors.Is(err, db.ErrContentExists) {
			// Lost a race with a concurrent identical upload.
			existing, err2 := u.repo.AvailableBySHA(ctx, staged.SHA256)
			if err2 != nil || existing == nil {
				return nil, false, fmt.Errorf("dedupe race: %w", err2)
			}
			return existing, true, nil
		}
		return nil, false, fmt.Errorf("create row: %w", err)
	}

	// Atomic commit; on failure the row is removed (no staged-row residue —
	// the sweeper would handle it, but immediate cleanup is cheaper).
	if _, err := u.store.Save(ctx, staged, a.Filename, a.ContentType); err != nil {
		discard()
		_ = u.repo.Delete(ctx, a.ID)
		return nil, false, fmt.Errorf("commit object: %w", err)
	}

	if err := u.repo.MarkAvailable(ctx, a.ID); err != nil {
		return a, false, fmt.Errorf("mark available: %w", err)
	}
	a.Status = db.StatusAvailable
	return a, false, nil
}

// sanitizeFilename keeps a single path-safe base name for metadata display;
// the stored object path is content-derived and never client-controlled.
func sanitizeFilename(name string) string {
	name = strings.TrimSpace(filepath.Base(name))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return -1
		}
		return r
	}, name)

	name = strings.Trim(name, ". ")
	if name == "" || !utf8.ValidString(name) {
		return "attachment"
	}
	if len(name) > 255 {
		name = name[:255]
	}
	return name
}
