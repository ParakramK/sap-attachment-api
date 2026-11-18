// Package storage commits staged uploads to a backend and manages committed
// objects. Backends must make Save atomic: either the object is fully present
// or not at all.
package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/parakram/sap-attachment-api/internal/config"
)

// Storage is the minimal backend surface needed by the service. One
// implementation per backend; nothing more.
type Storage interface {
	// Save commits a staged file to the path derived from its content hash and
	// filename, returning the backend-neutral relative object path.
	Save(ctx context.Context, staged *StagedFile, filename, contentType string) (rel string, err error)
	// Has reports whether the object at rel exists.
	Has(ctx context.Context, rel string) (bool, error)
	// Delete removes the object. Deleting a missing object is not an error.
	Delete(ctx context.Context, rel string) error
	// Reference returns the consumer-facing location of an object (absolute
	// disk path for local, URI for S3).
	Reference(rel string) string
}

// New builds the configured backend.
func New(cfg *config.Config) (Storage, error) {
	switch cfg.StorageBackend {
	case "local":
		return NewLocal(cfg.StorageDir)
	case "s3":
		return NewS3(cfg)
	default:
		return nil, fmt.Errorf("unknown storage backend %q", cfg.StorageBackend)
	}
}

// ErrTooLarge signals an upload exceeding the configured size limit.
var ErrTooLarge = errors.New("upload exceeds maximum size")
