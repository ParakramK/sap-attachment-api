package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/parakram/sap-attachment-api/internal/config"
)

// Storage is one storage backend. Every implementation must make Save
// atomic: readers see either nothing or the complete object.
type Storage interface {
	// Save commits a staged file to the path derived from its content hash
	// and filename, returning the backend-neutral relative object path.
	// It consumes the staged file on every path (moves it, or copies then
	// removes it).
	Save(ctx context.Context, staged *StagedFile, filename, contentType string) (rel string, err error)
	// Has reports whether the object at rel exists.
	Has(ctx context.Context, rel string) (bool, error)
	// Delete removes the object. Deleting a missing object is not an error.
	Delete(ctx context.Context, rel string) error
	// Reference returns the consumer-facing location of an object: the
	// absolute disk path for local, a Windows UNC path for smb.
	Reference(rel string) string
	// Name identifies the backend in the attachments table.
	Name() string
	// Close releases backend resources (SMB session); nil-safe to call.
	Close() error
}

// New builds the configured backend.
func New(cfg *config.Config) (Storage, error) {
	switch cfg.StorageBackend {
	case "local":
		return NewLocal(cfg.StorageDir)
	case "smb":
		return NewSMB(cfg)
	default:
		return nil, fmt.Errorf("unknown storage backend %q", cfg.StorageBackend)
	}
}

// ErrTooLarge signals an upload exceeding the configured size limit.
var ErrTooLarge = errors.New("upload exceeds maximum size")
