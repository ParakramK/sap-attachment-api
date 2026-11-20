// Package sap registers committed files as attachments in SAP Business One
// through its Service Layer REST API.
package sap

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"time"

	"github.com/parakram/sap-attachment-api/internal/config"
)

const requestTimeout = 30 * time.Second

// SAP sessions expire after ~30 minutes; re-login at 25 to keep them warm.
// A var so tests can shorten the interval.
var refreshInterval = 25 * time.Minute

const maxErrorBody = 512

// Client talks to the SAP Service Layer. A nil *Client means the integration
// is not configured and uploads simply skip it.
type Client struct {
	base     string
	database string
	username string
	password string
	http     *http.Client // carries the SESSIONID cookie from login to calls

	mu        sync.Mutex
	lastLogin time.Time
}

// New returns nil (no error) when SAP settings are absent.
func New(cfg *config.Config) (*Client, error) {
	if cfg.SAPServiceLayerURL == "" {
		return nil, nil
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.SAPInsecureTLS {
		// Off by default; opt-in only for SAP systems with self-signed certs.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // G402: explicit opt-in
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("sap cookie jar: %w", err)
	}
	return &Client{
		base:     strings.TrimRight(cfg.SAPServiceLayerURL, "/"),
		database: cfg.SAPDatabase,
		username: cfg.SAPUsername,
		password: cfg.SAPPassword,
		http:     &http.Client{Transport: transport, Jar: jar, Timeout: requestTimeout},
	}, nil
}

type attachmentLine struct {
	FileExtension string `json:"FileExtension"`
	FileName      string `json:"FileName"`
	SourcePath    string `json:"SourcePath"`
}

type attachmentRequest struct {
	Lines []attachmentLine `json:"Attachments2_Lines"`
}

// Login authenticates immediately (used at startup). Safe to call more than
// once: an active session is reused.
func (c *Client) Login(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	return c.ensureSession(ctx)
}

// Start logs in immediately and refreshes the session every 25 minutes, so
// it stays warm even when no uploads arrive. It returns when ctx is canceled.
// A failed refresh is ignored here: the next upload retries the login.
func (c *Client) Start(ctx context.Context) {
	if c == nil {
		return
	}
	c.refresh(ctx)
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refresh(ctx)
		}
	}
}

func (c *Client) refresh(ctx context.Context) {
	refreshCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	_ = c.ensureSession(refreshCtx)
}

// ensureSession reuses the current session until it approaches expiry.
func (c *Client) ensureSession(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.lastLogin.IsZero() && time.Since(c.lastLogin) < refreshInterval {
		return nil
	}
	if err := c.login(ctx); err != nil {
		return err
	}
	c.lastLogin = time.Now()
	return nil
}

// CreateAttachments registers all stored files in one Attachments2 call and
// returns SAP's AbsoluteEntry ids in the same order. SAP needs all three line
// fields: SourcePath (directory), FileName (stored key), FileExtension.
// entries for each line: SAP returns one top-level AbsoluteEntry for the
// whole collection, with no per-line ids in the usual response.
func (c *Client) CreateAttachments(ctx context.Context, storagePaths []string) ([]int64, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	if err := c.ensureSession(ctx); err != nil {
		return nil, err
	}

	lines := make([]attachmentLine, 0, len(storagePaths))
	for _, p := range storagePaths {
		dir, name, ext := splitPath(p)
		lines = append(lines, attachmentLine{FileExtension: ext, FileName: name, SourcePath: dir})
	}
	payload, err := json.Marshal(attachmentRequest{Lines: lines})
	if err != nil {
		return nil, fmt.Errorf("sap encode attachment: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/b1s/v1/Attachments2", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("sap attachment request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sap attachment call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("sap attachment status %d: %s", resp.StatusCode, errorBody(resp))
	}

	var result struct {
		AbsoluteEntry int64 `json:"AbsoluteEntry"`
		Lines         []struct {
			AbsoluteEntry int64 `json:"AbsoluteEntry"`
		} `json:"Attachments2_Lines"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("sap attachment decode: %w", err)
	}

	entries := make([]int64, len(lines))
	if len(result.Lines) == len(lines) {
		for i, line := range result.Lines {
			entries[i] = line.AbsoluteEntry
		}
	}
	if result.AbsoluteEntry != 0 {
		for i := range entries {
			if entries[i] == 0 {
				entries[i] = result.AbsoluteEntry
			}
		}
	}
	for i, entry := range entries {
		if entry == 0 {
			return nil, fmt.Errorf("sap attachment response missing AbsoluteEntry for line %d", i+1)
		}
	}
	return entries, nil
}

func (c *Client) login(ctx context.Context) error {
	credentials, err := json.Marshal(map[string]string{
		"CompanyDB": c.database,
		"UserName":  c.username,
		"Password":  c.password,
	})
	if err != nil {
		return fmt.Errorf("sap encode login: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/b1s/v1/Login", bytes.NewReader(credentials))
	if err != nil {
		return fmt.Errorf("sap login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sap login call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("sap login status %d: %s", resp.StatusCode, errorBody(resp))
	}
	return nil // cookie jar keeps the SESSIONID for the next call
}

// splitPath handles both POSIX paths and Windows UNC paths, which use
// different separators. It returns the directory, base name (no extension)
// and extension (no dot).
func splitPath(p string) (dir, name, ext string) {
	i := strings.LastIndexAny(p, `/\`)
	if i < 0 {
		dir, name = ".", p
	} else {
		dir, name = p[:i], p[i+1:]
	}
	if dot := strings.LastIndex(name, "."); dot > 0 {
		ext, name = name[dot+1:], name[:dot]
	}
	return dir, name, ext
}

func errorBody(resp *http.Response) string {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	return strings.TrimSpace(string(data))
}
