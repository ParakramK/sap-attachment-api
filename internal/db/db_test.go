package db

import (
	"context"
	"testing"
	"time"

	"github.com/parakram/sap-attachment-api/internal/config"
)

func testDB(t *testing.T) (*Repo, context.Context) {
	t.Helper()
	cfg := &config.Config{DBPath: t.TempDir() + "/test.db"}
	d, err := Open(cfg)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return NewRepo(d), context.Background()
}

func sample(t *testing.T, sha, status string) *Attachment {
	t.Helper()
	now := time.Now().Truncate(time.Second) // sqlite timestamp precision
	a := NewAttachment()
	a.Filename = "report.pdf"
	a.ContentType = "application/pdf"
	a.SizeBytes = 1234
	a.SHA256 = sha
	a.Status = status
	a.Backend = "local"
	a.ObjectPath = sha[:2] + "/" + sha + ".pdf"
	a.StoragePath = "/data/storage/" + a.ObjectPath
	a.CreatedAt = now
	a.UpdatedAt = now
	return a
}

func TestCreateAndRead(t *testing.T) {
	repo, ctx := testDB(t)

	a := sample(t, "aa11", StatusStaged)
	if err := repo.Create(ctx, a); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.ByID(ctx, a.ID)
	if err != nil || got == nil {
		t.Fatalf("byID: %v (got=%v)", err, got)
	}
	if got.Filename != "report.pdf" || got.SizeBytes != 1234 {
		t.Fatalf("unexpected row: %+v", got)
	}
	if got.StoragePath != a.StoragePath {
		t.Fatalf("storage_path round-trip failed: %q != %q", got.StoragePath, a.StoragePath)
	}
	if !got.CreatedAt.Equal(a.CreatedAt) {
		t.Fatalf("time round-trip failed: %v != %v", got.CreatedAt, a.CreatedAt)
	}
}

func TestContentDedupeEnforcedByIndex(t *testing.T) {
	repo, ctx := testDB(t)

	a := sample(t, "bb22", StatusAvailable)
	if err := repo.Create(ctx, a); err != nil {
		t.Fatalf("create first: %v", err)
	}

	dup := sample(t, "bb22", StatusAvailable)
	err := repo.Create(ctx, dup)
	if err == nil {
		t.Fatal("expected ErrContentExists for duplicate available hash")
	}

	// Same hash while still staged must NOT collide (partial index).
	staged := sample(t, "bb22", StatusStaged)
	if err := repo.Create(ctx, staged); err != nil {
		t.Fatalf("staged duplicate should be allowed: %v", err)
	}
}

func TestAvailableBySHA(t *testing.T) {
	repo, ctx := testDB(t)

	got, err := repo.AvailableBySHA(ctx, "cc33")
	if err != nil || got != nil {
		t.Fatalf("empty lookup: %v (got=%v)", err, got)
	}

	a := sample(t, "cc33", StatusAvailable)
	if err := repo.Create(ctx, a); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err = repo.AvailableBySHA(ctx, "cc33")
	if err != nil || got == nil {
		t.Fatalf("lookup: %v (got=%v)", err, got)
	}

	// Staged rows are not dedupe candidates.
	_ = repo.Delete(ctx, a.ID)
	if err := repo.Create(ctx, sample(t, "cc33", StatusStaged)); err != nil {
		t.Fatalf("create staged: %v", err)
	}
	if got, err = repo.AvailableBySHA(ctx, "cc33"); err != nil || got != nil {
		t.Fatalf("staged row must not match: %v (got=%v)", err, got)
	}
}

func TestMarkAvailableAndDelete(t *testing.T) {
	repo, ctx := testDB(t)

	a := sample(t, "dd44", StatusStaged)
	if err := repo.Create(ctx, a); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.MarkAvailable(ctx, a.ID); err != nil {
		t.Fatalf("mark available: %v", err)
	}
	got, _ := repo.ByID(ctx, a.ID)
	if got.Status != StatusAvailable {
		t.Fatalf("status = %q, want available", got.Status)
	}

	if err := repo.Delete(ctx, a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got, _ = repo.ByID(ctx, a.ID); got != nil {
		t.Fatal("row still present after delete")
	}
	// Delete is idempotent.
	if err := repo.Delete(ctx, a.ID); err != nil {
		t.Fatalf("re-delete: %v", err)
	}
}

func TestStagedOlderThan(t *testing.T) {
	repo, ctx := testDB(t)

	old := sample(t, "ee55", StatusStaged)
	old.CreatedAt = time.Now().Add(-2 * time.Hour)
	old.UpdatedAt = old.CreatedAt
	if err := repo.Create(ctx, old); err != nil {
		t.Fatalf("create old: %v", err)
	}
	_ = repo.Create(ctx, sample(t, "ee66", StatusStaged)) // fresh

	rows, err := repo.StagedOlderThan(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("staged older than: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != old.ID {
		t.Fatalf("expected only the old row, got %+v", rows)
	}
}
