package service

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/parakram/sap-attachment-api/internal/config"
	"github.com/parakram/sap-attachment-api/internal/db"
	"github.com/parakram/sap-attachment-api/internal/storage"
)

type fixture struct {
	uploader *Uploader
	repo     *db.Repo
	d        *bun.DB
	store    storage.Storage
	sweeper  *Sweeper
	dir      string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store, err := storage.NewLocal(filepath.Join(t.TempDir(), "storage"))
	if err != nil {
		t.Fatal(err)
	}
	return newFixtureWithBackend(t, store)
}

func newFixtureWithBackend(t *testing.T, backend storage.Storage) *fixture {
	t.Helper()
	cfg := &config.Config{DBPath: t.TempDir() + "/test.db"}
	d, err := db.Open(cfg)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	dir := t.TempDir()
	staging := filepath.Join(dir, "staging")

	repo := db.NewRepo(d)
	u, err := NewUploader(repo, backend, staging, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	// Deterministic timestamps for assertions.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var seq int
	u.now = func() time.Time { seq++; return base.Add(time.Duration(seq) * time.Second) }

	return &fixture{
		uploader: u,
		repo:     repo,
		d:        d,
		store:    backend,
		sweeper:  NewSweeper(repo, backend, staging, time.Hour),
		dir:      dir,
	}
}

func TestUploadDirect(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	pdf := append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte("x"), 2048)...)
	att, deduped, err := f.uploader.Upload(ctx, Input{
		Reader:   bytes.NewReader(pdf),
		Filename: "../weird/../../report.pdf\n",
		Size:     -1,
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if deduped {
		t.Fatal("first upload must not be deduped")
	}
	if att.Filename != "report.pdf" { // path traversal + control chars stripped
		t.Fatalf("filename = %q", att.Filename)
	}
	if att.ContentType != "application/pdf" {
		t.Fatalf("sniffed content type = %q, want application/pdf", att.ContentType)
	}
	if att.Status != db.StatusAvailable || att.Backend != "local" {
		t.Fatalf("unexpected status/backend: %s %s", att.Status, att.Backend)
	}

	// Object is on disk at the referenced path with the original bytes.
	got, err := f.store.Has(ctx, att.ObjectPath)
	if err != nil || !got {
		t.Fatalf("object missing: %v %v", err, got)
	}
	abs := f.store.Reference(att.ObjectPath)
	if !filepath.IsAbs(abs) {
		t.Fatalf("reference not absolute: %q", abs)
	}

	// Staging dir is empty after commit.
	entries, _ := filepath.Glob(filepath.Join(f.dir, "staging", "*"))
	if len(entries) != 0 {
		t.Fatalf("staging dir not clean: %v", entries)
	}
}

func TestUploadSniffsRealContentType(t *testing.T) {
	f := newFixture(t)
	png := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, bytes.Repeat([]byte{0}, 600)...)

	att, _, err := f.uploader.Upload(context.Background(), Input{
		Reader:   bytes.NewReader(png),
		Filename: "declared-as-text.txt", // lying name must not win
	})
	if err != nil {
		t.Fatal(err)
	}
	if att.ContentType != "image/png" {
		t.Fatalf("content type = %q, want image/png", att.ContentType)
	}
	if att.Filename != "declared-as-text.txt" {
		t.Fatalf("filename = %q", att.Filename)
	}
}

func TestUploadDedupeReturnsExisting(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	content := []byte("identical content")

	first, deduped, err := f.uploader.Upload(ctx, Input{Reader: bytes.NewReader(content), Filename: "first.txt"})
	if err != nil || deduped {
		t.Fatalf("first: err=%v deduped=%v", err, deduped)
	}

	second, deduped, err := f.uploader.Upload(ctx, Input{Reader: bytes.NewReader(content), Filename: "second.txt"})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !deduped {
		t.Fatal("second upload should be deduped")
	}
	if second.ID != first.ID || second.Filename != "first.txt" {
		t.Fatalf("expected original record, got %+v", second)
	}
}

func TestUploadOversized(t *testing.T) {
	f := newFixture(t)
	huge := bytes.Repeat([]byte("a"), 2<<20) // > 1MB cap in fixture

	_, _, err := f.uploader.Upload(context.Background(), Input{Reader: bytes.NewReader(huge), Filename: "big.bin"})
	if !errors.Is(err, storage.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestUploadEmptyFile(t *testing.T) {
	f := newFixture(t)
	att, _, err := f.uploader.Upload(context.Background(), Input{Reader: strings.NewReader(""), Filename: "empty.txt"})
	if err != nil {
		t.Fatalf("empty upload: %v", err)
	}
	if att.SizeBytes != 0 {
		t.Fatalf("size = %d, want 0", att.SizeBytes)
	}
}

func TestUploadUsesIdempotencyKeyAsFilename(t *testing.T) {
	f := newFixture(t)
	key := "11111111-2222-4333-8444-555555555555"
	longName := strings.Repeat("long-client-name-", 30) + ".PDF"

	att, _, err := f.uploader.Upload(context.Background(), Input{
		Reader:         strings.NewReader("%PDF-1.4 content"),
		Filename:       longName,
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := key + ".pdf" // extension from the original file, lowercased
	if att.Filename != want || !strings.HasSuffix(att.ObjectPath, "/"+want) {
		t.Fatalf("filename = %q, path = %q, want name %q", att.Filename, att.ObjectPath, want)
	}
	if len(att.Filename) > 60 {
		t.Fatalf("filename too long: %q", att.Filename)
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd":  "passwd",
		"a/b/c.png":         "c.png",
		"with space.pdf":    "with space.pdf",
		"ctrl\x00name":      "ctrlname",
		"trim..":            "trim",
		"":                  "attachment",
		"   ":               "attachment",
		"über-fileäöü.docx": "über-fileäöü.docx",
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}
