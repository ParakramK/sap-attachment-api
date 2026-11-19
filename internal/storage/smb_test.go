package storage

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/parakram/sap-attachment-api/internal/config"
)

func smbConfig(server string) *config.Config {
	return &config.Config{
		StorageBackend:    "smb",
		SMBServer:         server,
		SMBShare:          "attachment",
		SMBUsername:       "svc",
		SMBPassword:       "secret",
		SMBRequireSigning: true,
		SMBStagingDir:     "staging",
	}
}

// NewSMB must not dial; these tests exercise construction and path mapping.
func TestSMBReferenceIsUNCWithoutPort(t *testing.T) {
	for _, tc := range []struct{ server, wantHost string }{
		{"192.168.1.10", "192.168.1.10"},
		{"192.168.1.10:445", "192.168.1.10"},
		{"nas.corp.local:1445", "nas.corp.local"},
	} {
		s, err := NewSMB(smbConfig(tc.server))
		if err != nil {
			t.Fatalf("NewSMB(%q): %v", tc.server, err)
		}
		got := s.Reference("ab/abcdef.pdf")
		want := `\\` + tc.wantHost + `\attachment\ab\abcdef.pdf`
		if got != want {
			t.Errorf("Reference after %q = %q, want %q", tc.server, got, want)
		}
	}
}

func TestSMBBaseDirNestsObjectsAndStaging(t *testing.T) {
	cfg := smbConfig("192.168.1.10")
	cfg.SMBShare = "nas" // \\IP\nas\attachments
	cfg.SMBBaseDir = "attachments"
	s, err := NewSMB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.base != "attachments" || s.staging != "attachments/staging" {
		t.Fatalf("base=%q staging=%q", s.base, s.staging)
	}
	// The DB stores a base-free object_path; the backend resolves the base
	// for reads, deletes and UNC references.
	if got := s.objectPath("abcdef.pdf"); got != "attachments/abcdef.pdf" {
		t.Fatalf("objectPath = %q", got)
	}
	got := s.Reference("abcdef.pdf")
	want := `\\192.168.1.10\nas\attachments\abcdef.pdf`
	if got != want {
		t.Fatalf("reference = %q, want %q", got, want)
	}
}

func TestSMBEmptyServerRejected(t *testing.T) {
	if _, err := NewSMB(smbConfig(":445")); err == nil {
		t.Fatal("expected error for server-less address")
	}
}

func TestSMBName(t *testing.T) {
	s, err := NewSMB(smbConfig("nas"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name() != "smb" {
		t.Fatalf("name = %q", s.Name())
	}
}

// TestSMBInterfaceConformance guards the Save/Has/Delete/Reference/Close
// contract with a compile-time check and a no-server runtime check.
func TestSMBInterfaceConformance(t *testing.T) {
	var _ Storage = (*SMB)(nil)
	var _ Storage = (*Local)(nil)
}

// TestSMBLiveCycle runs only against a real SMB server, configured via:
//
//	SMB_TEST_ADDR=host:port SMB_TEST_SHARE=share \
//	SMB_TEST_USER=user SMB_TEST_PASS=secret go test -run TestSMBLiveCycle
func TestSMBLiveCycle(t *testing.T) {
	addr := os.Getenv("SMB_TEST_ADDR")
	if addr == "" {
		t.Skip("SMB_TEST_ADDR not set; skipping live SMB test")
	}

	cfg := smbConfig(addr)
	cfg.SMBShare = os.Getenv("SMB_TEST_SHARE")
	cfg.SMBUsername = os.Getenv("SMB_TEST_USER")
	cfg.SMBPassword = os.Getenv("SMB_TEST_PASS")

	s, err := NewSMB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	// Stage locally, exactly like the uploader does.
	staged, err := Stage(t.TempDir(), strings.NewReader("smb live content"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	rel, err := s.Save(ctx, staged, "live-test.txt", "text/plain")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	t.Logf("rel=%q reference=%q", rel, s.Reference(rel))

	if has, err := s.Has(ctx, rel); err != nil || !has {
		t.Fatalf("has: %v %v", has, err)
	}
	if err := s.Delete(ctx, rel); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if has, err := s.Has(ctx, rel); err != nil || has {
		t.Fatalf("has after delete: %v %v", has, err)
	}
}
