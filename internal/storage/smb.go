package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path"
	"strings"
	"sync"

	smb2 "github.com/cloudsoda/go-smb2"

	"github.com/parakram/sap-attachment-api/internal/config"
)

// SMB stores objects on a remote SMB2/3 share, speaking the protocol
// directly (no OS mount). Staged files are copied into a staging directory
// on the same share and atomically renamed into place, so readers see
// either nothing or the complete object.
type SMB struct {
	host     string // server without port, used in UNC references
	port     string
	share    string
	username string
	password string
	domain   string
	signing  bool
	base     string // share-relative folder holding all objects (may be empty)
	staging  string // share-relative staging dir, slash-separated

	mu      sync.Mutex
	mounted *smb2.Share
	session *smb2.Session
}

func NewSMB(cfg *config.Config) (*SMB, error) {
	host, port, err := net.SplitHostPort(cfg.SMBServer)
	if err != nil {
		host, port = strings.TrimSpace(cfg.SMBServer), "445"
	}
	if host == "" {
		return nil, errors.New("SMB_SERVER must not be empty")
	}
	base := strings.TrimSuffix(strings.ReplaceAll(cfg.SMBBaseDir, `\`, "/"), "/")
	staging := strings.TrimSuffix(strings.ReplaceAll(cfg.SMBStagingDir, `\`, "/"), "/")
	if base != "" {
		staging = base + "/" + staging // keep all service data under the base folder
	}
	return &SMB{
		host:     host,
		port:     port,
		share:    cfg.SMBShare,
		username: cfg.SMBUsername,
		password: cfg.SMBPassword,
		domain:   cfg.SMBDomain,
		signing:  cfg.SMBRequireSigning,
		base:     base,
		staging:  staging,
	}, nil
}

// mount returns the connected share, dialing on first use. *smb2.Share is
// safe for concurrent use.
func (s *SMB) mount(ctx context.Context) (*smb2.Share, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mounted != nil {
		return s.mounted, nil
	}

	dialer := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{
			User:     s.username,
			Password: s.password,
			Domain:   s.domain,
		},
		Negotiator: smb2.Negotiator{RequireMessageSigning: s.signing},
	}
	session, err := dialer.Dial(ctx, net.JoinHostPort(s.host, s.port))
	if err != nil {
		return nil, fmt.Errorf("smb dial %s: %w", s.host, err)
	}
	sh, err := session.Mount(s.share)
	if err != nil {
		_ = session.Logoff()
		return nil, fmt.Errorf("smb mount %q: %w", s.share, err)
	}
	s.session, s.mounted = session, sh
	return sh, nil
}

// drop tears the session down so the next operation redials.
func (s *SMB) drop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session != nil {
		_ = s.session.Logoff()
	}
	s.session, s.mounted = nil, nil
}

// withShare runs op against a connected share. SMB sessions die silently
// (NAS restarts, idle drops): a failed operation triggers exactly one
// redial-and-retry before the error is surfaced.
func (s *SMB) withShare(ctx context.Context, op func(sh *smb2.Share) error) error {
	sh, err := s.mount(ctx)
	if err != nil {
		return err
	}
	if err = op(sh.WithContext(ctx)); err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return err // app-level miss, not a broken transport
	}

	s.drop()
	sh, err = s.mount(ctx)
	if err != nil {
		return err
	}
	return op(sh.WithContext(ctx))
}

// objectPath resolves a backend-neutral, base-free object path (the value
// stored in the DB) to its location inside the share's base folder.
func (s *SMB) objectPath(rel string) string { return path.Join(s.base, rel) }

// Save copies the staged file into the share's staging dir and renames it
// into place (ReplaceIfExists), which is the atomic commit. A leftover
// object from an earlier crash is simply overwritten. The returned rel stays
// base-free so it matches the DB's object_path.
func (s *SMB) Save(ctx context.Context, staged *StagedFile, filename, _ string) (string, error) {
	defer func() { _ = staged.Discard() }()

	// The idempotency key (plus its bounded extension) is the whole file
	// name; the optional SMB base folder is applied by objectPath.
	rel := filename
	target := s.objectPath(rel)
	tmp := s.staging + "/" + staged.SHA256 + "-" + randToken() + ".tmp"

	err := s.withShare(ctx, func(sh *smb2.Share) error {
		if err := sh.MkdirAll(s.staging, 0o750); err != nil {
			return fmt.Errorf("smb create staging dir: %w", err)
		}
		if dir := path.Dir(target); dir != "." {
			if err := sh.MkdirAll(dir, 0o750); err != nil {
				return fmt.Errorf("smb create object dir: %w", err)
			}
		}

		src, err := os.Open(staged.Path) //nolint:gosec // G304: path is internally derived
		if err != nil {
			return fmt.Errorf("open staged file: %w", err)
		}
		defer func() { _ = src.Close() }()

		dst, err := sh.Create(tmp)
		if err != nil {
			return fmt.Errorf("smb create staged object: %w", err)
		}
		if _, err := dst.ReadFrom(src); err != nil {
			_ = dst.Close()
			return fmt.Errorf("smb write staged object: %w", err)
		}
		if err := dst.Sync(); err != nil {
			_ = dst.Close()
			return fmt.Errorf("smb sync staged object: %w", err)
		}
		if err := dst.Close(); err != nil {
			return fmt.Errorf("smb close staged object: %w", err)
		}

		if err := sh.Rename(tmp, target); err != nil {
			return fmt.Errorf("smb commit object: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return rel, nil
}

func (s *SMB) Has(ctx context.Context, rel string) (bool, error) {
	var has bool
	err := s.withShare(ctx, func(sh *smb2.Share) error {
		_, err := sh.Stat(s.objectPath(rel))
		switch {
		case err == nil:
			has = true
		case errors.Is(err, fs.ErrNotExist):
			// miss
		default:
			return err
		}
		return nil
	})
	return has, err
}

func (s *SMB) Delete(ctx context.Context, rel string) error {
	return s.withShare(ctx, func(sh *smb2.Share) error {
		if err := sh.Remove(s.objectPath(rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
}

// Reference is the Windows UNC path the SAP integration reads the file from.
func (s *SMB) Reference(rel string) string {
	full := s.objectPath(rel)
	return `\\` + s.host + `\` + s.share + `\` + strings.ReplaceAll(full, "/", `\`)
}

func (s *SMB) Name() string { return "smb" }

func (s *SMB) Close() error { s.drop(); return nil }

// randToken disambiguates concurrent saves of identical content.
func randToken() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
