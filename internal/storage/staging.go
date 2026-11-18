package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

// StagedFile is an uploaded body written to a temp file in the staging
// directory, with its content digest and size computed while streaming.
type StagedFile struct {
	Path   string // temp file location
	Size   int64
	SHA256 string // hex digest
}

// Discard removes the staged temp file. Missing file is not an error.
func (s *StagedFile) Discard() error {
	if err := os.Remove(s.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Stage streams r into a fresh temp file under dir, capped at maxBytes
// (exceeding it fails with ErrTooLarge). The temp file is synced before
// returning so a later rename is durable.
func Stage(dir string, r io.Reader, maxBytes int64) (*StagedFile, error) {
	tmp, err := os.CreateTemp(dir, "upload-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create staging file: %w", err)
	}

	staged := &StagedFile{Path: tmp.Name()}
	hasher := sha256.New()
	sink := io.MultiWriter(tmp, hasher)

	size, err := io.Copy(sink, io.LimitReader(r, maxBytes+1))
	if err == nil && size > maxBytes {
		err = ErrTooLarge
	}
	if err != nil {
		_ = tmp.Close()
		_ = staged.Discard()
		return nil, err
	}

	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = staged.Discard()
		return nil, fmt.Errorf("sync staging file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = staged.Discard()
		return nil, fmt.Errorf("close staging file: %w", err)
	}

	staged.Size = size
	staged.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	return staged, nil
}
