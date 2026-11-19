package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/parakram/sap-attachment-api/internal/db"
	"github.com/parakram/sap-attachment-api/internal/storage"
)

// fakeBackend records object presence so sweeper decisions can be driven
// without a real storage server (covers the SMB backend's sweep path).
type fakeBackend struct {
	present map[string]bool
}

func newFakeBackend() *fakeBackend { return &fakeBackend{present: map[string]bool{}} }

func (f *fakeBackend) Save(_ context.Context, staged *storage.StagedFile, filename, _ string) (string, error) {
	rel := filename
	f.present[rel] = true
	_ = staged.Discard()
	return rel, nil
}
func (f *fakeBackend) Has(_ context.Context, rel string) (bool, error) { return f.present[rel], nil }
func (f *fakeBackend) Delete(_ context.Context, rel string) error      { delete(f.present, rel); return nil }
func (f *fakeBackend) Reference(rel string) string                     { return "/fake/" + rel }
func (f *fakeBackend) Name() string                                    { return "fake" }
func (f *fakeBackend) Close() error                                    { return nil }

func TestSweeperResolvesStagedRowsViaBackend(t *testing.T) {
	backend := newFakeBackend()
	f := newFixtureWithBackend(t, backend)
	ctx := context.Background()

	// Two interrupted uploads: one whose object reached storage, one lost.
	reached, _, err := f.uploader.Upload(ctx, Input{Reader: strings.NewReader("%PDF-1.4 reached content"), Filename: "a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	lost, _, err := f.uploader.Upload(ctx, Input{Reader: strings.NewReader("%PDF-1.4 lost content"), Filename: "b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate "object never landed" for the second row.
	delete(backend.present, lost.ObjectPath)
	// And "row stuck in staged" for both (uploads normally flip the status).
	old := time.Now().Add(-2 * time.Hour) // older than the sweeper's max age
	if _, err := f.d.NewUpdate().
		Model((*db.Attachment)(nil)).
		Set("status = ?", db.StatusStaged).
		Set("created_at = ?", old).
		Set("updated_at = ?", old).
		Where("1 = 1").
		Exec(ctx); err != nil {
		t.Fatal(err)
	}

	res, err := f.sweeper.Run(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.ResolvedRows != 2 {
		t.Fatalf("resolved = %d, want 2", res.ResolvedRows)
	}

	gotReached, err := f.repo.ByID(ctx, reached.ID)
	if err != nil || gotReached == nil || gotReached.Status != db.StatusAvailable {
		t.Fatalf("reached row not marked available: %v %+v", err, gotReached)
	}
	if gotLost, err := f.repo.ByID(ctx, lost.ID); err != nil || gotLost != nil {
		t.Fatalf("lost row not deleted: %v %+v", err, gotLost)
	}
}
