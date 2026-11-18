package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newLocal(t *testing.T) *Local {
	t.Helper()
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func stagedContent(t *testing.T, dir, content string) *StagedFile {
	t.Helper()
	s, err := Stage(dir, bytes.NewReader([]byte(content)), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Discard() })
	return s
}

func TestExtSanitization(t *testing.T) {
	cases := map[string]string{
		"report.pdf":       ".pdf",
		"IMAGE.PNG":        ".png",
		"no extension":     "",
		"weird../etc":      "",
		"many.dots.doc":    ".doc",
		".hidden":          ".hidden",
		"long.a1b2c3d4e5f": "",
	}
	for in, want := range cases {
		if got := Ext(in); got != want {
			t.Errorf("Ext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestObjectPathLayout(t *testing.T) {
	got := ObjectPath(strings.Repeat("a", 64), "file.tar.gz")
	if !strings.HasPrefix(got, "aa/") || !strings.HasSuffix(got, ".gz") {
		t.Fatalf("unexpected path %q", got)
	}
	if n := strings.Count(got, "/"); n != 1 {
		t.Fatalf("path traversal risk in %q", got)
	}
}

// TestNewLocalResolvesRelativeRoot: the API contract promises an absolute
// storage_path even when STORAGE_DIR is relative to the working directory.
func TestNewLocalResolvesRelativeRoot(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	l, err := NewLocal("./storage/under/cwd")
	if err != nil {
		t.Fatal(err)
	}
	if ref := l.Reference("aa/abc.pdf"); !filepath.IsAbs(ref) {
		t.Fatalf("reference = %q, want absolute", ref)
	}
}

func TestLocalSaveHasDeleteReference(t *testing.T) {
	l := newLocal(t)
	ctx := context.Background()
	dir := t.TempDir()

	staged := stagedContent(t, dir, "file content")
	sha := staged.SHA256

	rel, err := l.Save(ctx, staged, "report.pdf", "application/pdf")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	wantRel := sha[:2] + "/" + sha + ".pdf"
	if rel != wantRel {
		t.Fatalf("rel = %q, want %q", rel, wantRel)
	}

	// Committed object exists at the reference and holds the content.
	ref := l.Reference(rel)
	data, err := os.ReadFile(ref) //nolint:gosec // G304: ref is an internally derived path
	if err != nil || string(data) != "file content" {
		t.Fatalf("reference unreadable: %v %q", err, data)
	}
	if !filepath.IsAbs(ref) {
		t.Fatalf("local reference should be absolute: %q", ref)
	}

	has, err := l.Has(ctx, rel)
	if err != nil || !has {
		t.Fatalf("has: %v %v", err, has)
	}

	// Save is atomic: the staged temp file no longer exists.
	if _, err := os.Stat(staged.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staged temp file should be gone after commit")
	}

	if err := l.Delete(ctx, rel); err != nil {
		t.Fatalf("delete: %v", err)
	}
	has, err = l.Has(ctx, rel)
	if err != nil || has {
		t.Fatalf("after delete: %v %v", err, has)
	}
	// Delete is idempotent.
	if err := l.Delete(ctx, rel); err != nil {
		t.Fatalf("re-delete: %v", err)
	}
}

func TestLocalSaveSameContentOverwrites(t *testing.T) {
	l := newLocal(t)
	ctx := context.Background()
	dir := t.TempDir()

	first := stagedContent(t, dir, "same content")
	if _, err := l.Save(ctx, first, "a.txt", "text/plain"); err != nil {
		t.Fatal(err)
	}

	second := stagedContent(t, dir, "same content")
	if _, err := l.Save(ctx, second, "b.txt", "text/plain"); err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256([]byte("same content"))
	has, _ := l.Has(ctx, ObjectPath(hex.EncodeToString(sum[:]), "a.txt"))
	if !has {
		t.Fatal("overwritten object missing")
	}
}
