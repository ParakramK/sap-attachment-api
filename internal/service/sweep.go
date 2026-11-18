// Package service holds cross-cutting service logic: upload orchestration
// (added with the API milestone) and storage/database reconciliation.
package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/parakram/sap-attachment-api/internal/db"
	"github.com/parakram/sap-attachment-api/internal/storage"
)

// Sweeper reconciles storage and the database:
//   - removes orphan temp files left in the staging directory by crashes
//   - resolves rows stuck in staged: object present -> mark available,
//     object absent -> delete the row
//
// Run at startup and on demand (admin endpoint) after crashes or upgrades.
type Sweeper struct {
	repo         *db.Repo
	backend      storage.Storage
	stagingDir   string
	maxStagedAge time.Duration
}

func NewSweeper(repo *db.Repo, backend storage.Storage, stagingDir string, maxStagedAge time.Duration) *Sweeper {
	return &Sweeper{repo: repo, backend: backend, stagingDir: stagingDir, maxStagedAge: maxStagedAge}
}

type SweepResult struct {
	RemovedTempFiles int
	ResolvedRows     int
}

func (s *Sweeper) Run(ctx context.Context) (SweepResult, error) {
	var result SweepResult

	n, err := s.cleanStaging()
	if err != nil {
		return result, fmt.Errorf("clean staging dir: %w", err)
	}
	result.RemovedTempFiles = n

	// In-flight uploads have staged rows younger than maxStagedAge; older ones
	// were interrupted.
	rows, err := s.repo.StagedOlderThan(ctx, time.Now().Add(-s.maxStagedAge))
	if err != nil {
		return result, fmt.Errorf("list staged rows: %w", err)
	}

	for _, row := range rows {
		has, err := s.backend.Has(ctx, row.ObjectPath)
		if err != nil {
			return result, fmt.Errorf("check object %q: %w", row.ObjectPath, err)
		}
		if has {
			err = s.repo.MarkAvailable(ctx, row.ID)
		} else {
			err = s.repo.Delete(ctx, row.ID)
		}
		if err != nil {
			return result, fmt.Errorf("resolve row %s: %w", row.ID, err)
		}
		result.ResolvedRows++
	}

	return result, nil
}

func (s *Sweeper) cleanStaging() (int, error) {
	entries, err := os.ReadDir(s.stagingDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		if err := os.Remove(filepath.Join(s.stagingDir, e.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
