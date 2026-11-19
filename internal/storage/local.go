package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Local stores objects on the local filesystem under a single root, using
// atomic renames from the staging directory (same filesystem requirement).
//
// Objects are always atomic: Save moves a synced staging file into place, so
// readers see either nothing or the complete object.
type Local struct {
	root string
}

func NewLocal(root string) (*Local, error) {
	// Resolve once: storage_path in API responses must be absolute even when
	// the configured root is relative to the working directory.
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve storage root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("create storage root: %w", err)
	}
	return &Local{root: abs}, nil
}

func (l *Local) Save(_ context.Context, staged *StagedFile, filename, _ string) (string, error) {
	rel := filename
	dest := filepath.Join(l.root, filepath.FromSlash(rel))

	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return "", fmt.Errorf("create object directory: %w", err)
	}
	if err := os.Rename(staged.Path, dest); err != nil {
		return "", fmt.Errorf("commit object: %w", err)
	}
	syncDir(filepath.Dir(dest))
	return rel, nil
}

func (l *Local) Has(_ context.Context, rel string) (bool, error) {
	_, err := os.Stat(filepath.Join(l.root, filepath.FromSlash(rel)))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (l *Local) Delete(_ context.Context, rel string) error {
	if err := os.Remove(filepath.Join(l.root, filepath.FromSlash(rel))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (l *Local) Reference(rel string) string {
	return filepath.Join(l.root, filepath.FromSlash(rel))
}

func (l *Local) Name() string { return "local" }

// Close releases backend resources; the local backend holds none.
func (l *Local) Close() error { return nil }

// syncDir flushes the directory entry so a rename survives a crash.
func syncDir(dir string) {
	d, err := os.Open(dir) //nolint:gosec // G304: dir is an internally derived path, never client input
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
