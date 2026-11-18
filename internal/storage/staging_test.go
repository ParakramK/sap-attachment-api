package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStageComputesDigestAndSize(t *testing.T) {
	dir := t.TempDir()
	content := []byte("hello attachment")
	want := sha256.Sum256(content)

	staged, err := Stage(dir, bytes.NewReader(content), 1<<20)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	t.Cleanup(func() { _ = staged.Discard() })

	if staged.Size != int64(len(content)) {
		t.Fatalf("size = %d, want %d", staged.Size, len(content))
	}
	if staged.SHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("sha mismatch: %s", staged.SHA256)
	}
	data, err := os.ReadFile(staged.Path)
	if err != nil || !bytes.Equal(data, content) {
		t.Fatalf("staged content wrong: %v %q", err, data)
	}
}

func TestStageRejectsOversizedUpload(t *testing.T) {
	dir := t.TempDir()
	content := bytes.Repeat([]byte("x"), 100)

	_, err := Stage(dir, bytes.NewReader(content), 50)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}

	// Exactly at the limit succeeds.
	staged, err := Stage(dir, bytes.NewReader(content), 100)
	if err != nil {
		t.Fatalf("at-limit stage failed: %v", err)
	}
	_ = staged.Discard()

	// No temp file leaks from failures (failed uploads clean up themselves).
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("expected 0 remaining temp files, got %d", len(entries))
	}
}

func TestDiscard(t *testing.T) {
	staged, err := Stage(t.TempDir(), bytes.NewReader([]byte("x")), 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := staged.Discard(); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if _, err := os.Stat(staged.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("file still present after discard")
	}
	// Discard is idempotent.
	if err := staged.Discard(); err != nil {
		t.Fatalf("re-discard: %v", err)
	}
}

func TestStageTempFileIsInDir(t *testing.T) {
	dir := t.TempDir()
	staged, err := Stage(dir, bytes.NewReader([]byte("y")), 10)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = staged.Discard() })
	if filepath.Dir(staged.Path) != dir {
		t.Fatalf("staged in %q, want %q", filepath.Dir(staged.Path), dir)
	}
}
