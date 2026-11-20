// Package sap registers committed files as attachments in SAP Business One
// through its Service Layer REST API.
package sap

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"

	"github.com/parakram/sap-attachment-api/internal/config"
)

const (
	requestTimeout = 30 * time.Second
	maxErrorBody   = 512
)

// Client talks to the SAP Service Layer. A nil *Client means the integration
// is not configured and uploads simply skip it.
type Client struct {
	base     string
	database string
	username string
	password string
	http     *http.Client // carries the SESSIONID cookie from login to calls
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

// CreateAttachment logs in and registers one stored file, returning SAP's
// AbsoluteEntry id. SAP needs all three line fields: SourcePath is the
// directory as seen by the SAP host, FileName the stored key and
// FileExtension its extension — with only SourcePath, SAP creates an entry
// with null name/extension.
//
// shortcut: logs in on every upload; cache the session when upload volume
// warrants it (and refresh on 401).
func (c *Client) CreateAttachment(ctx context.Context, storagePath string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	if err := c.login(ctx); err != nil {
		return 0, err
	}

	dir, name, ext := splitPath(storagePath)
	payload, err := json.Marshal(attachmentRequest{
		Lines: []attachmentLine{{FileExtension: ext, FileName: name, SourcePath: dir}},
	})
	if err != nil {
		return 0, fmt.Errorf("sap encode attachment: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+"/b1s/v1/Attachments2", bytes.NewReader(payload))
	if err != nil {
		return 0, fmt.Errorf("sap attachment request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("sap attachment call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("sap attachment status %d: %s", resp.StatusCode, errorBody(resp))
	}

	var result struct {
		AbsoluteEntry int64 `json:"AbsoluteEntry"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("sap attachment decode: %w", err)
	}
	if result.AbsoluteEntry == 0 {
		return 0, errors.New("sap attachment response missing AbsoluteEntry")
	}
	return result.AbsoluteEntry, nil
}

func (c *Client) login(ctx context.Context) error {
	credentials, err := json.Marshal(map[string]string{
		"CompanyDatabase": c.database,
		"UserName":        c.username,
		"Password":        c.password,
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
