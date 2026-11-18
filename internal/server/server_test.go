package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/parakram/sap-attachment-api/internal/config"
	"github.com/parakram/sap-attachment-api/internal/server"
)

func testConfig() *config.Config {
	return &config.Config{
		Env:                  "dev",
		Addr:                 ":0",
		LogLevel:             "info",
		StorageDir:           "./data/storage",
		StagingDir:           "./data/staging",
		DBPath:               "./data/test.db",
		S3Region:             "us-east-1",
		MaxUploadMB:          10,
		FetchTimeout:         5 * time.Second,
		FetchAllowPrivateNet: true, // tests fetch httptest loopback servers
		ShutdownTimeout:      time.Second,
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestOpenAPIAndDocsServed(t *testing.T) {
	app := server.New(server.Deps{
		Cfg:   testConfig(),
		Log:   testLogger(),
		Ready: func(context.Context) error { return nil },
	})

	resp := do(t, app, http.MethodGet, "/openapi.yaml")
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusOK ||
		!strings.Contains(body, "/v1/attachments") ||
		!strings.Contains(body, "X-API-Key") {
		t.Fatalf("openapi.yaml incomplete: status=%d len=%d", resp.StatusCode, len(body))
	}

	resp = do(t, app, http.MethodGet, "/docs")
	body = readAll(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "SwaggerUIBundle") {
		t.Fatalf("docs page broken: status=%d", resp.StatusCode)
	}
}

func do(t *testing.T, app *fiber.App, method, path string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}

func TestHealthz(t *testing.T) {
	app := server.New(server.Deps{
		Cfg:   testConfig(),
		Log:   testLogger(),
		Ready: func(context.Context) error { return nil },
	})
	resp := do(t, app, http.MethodGet, "/healthz")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "ok" {
		t.Fatalf("status field = %q, want %q", body.Status, "ok")
	}
}

func TestNotFoundUsesErrorEnvelope(t *testing.T) {
	app := server.New(server.Deps{
		Cfg:   testConfig(),
		Log:   testLogger(),
		Ready: func(context.Context) error { return nil },
	})
	resp := do(t, app, http.MethodGet, "/definitely-not-a-route")

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}

	var body struct {
		Code      int    `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Code != http.StatusNotFound || body.Message == "" {
		t.Fatalf("unexpected envelope: %+v", body)
	}
	if body.RequestID == "" {
		t.Fatal("request_id missing from error envelope")
	}
}

// Regression: the access log must report the post-error-handler status, not
// the 200 fasthttp defaults to before the error handler writes the response.
func TestAccessLogReflectsErrorStatus(t *testing.T) {
	var buf syncBuffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	app := server.New(server.Deps{
		Cfg:   testConfig(),
		Log:   log,
		Ready: func(context.Context) error { return nil },
	})

	do(t, app, http.MethodGet, "/definitely-not-a-route")

	line := buf.lastLine(t)
	var rec struct {
		Msg    string `json:"msg"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("decode log line: %v", err)
	}
	if rec.Msg != "http_request" {
		t.Fatalf("msg = %q, want http_request", rec.Msg)
	}
	if rec.Status != http.StatusNotFound {
		t.Fatalf("logged status = %d, want %d", rec.Status, http.StatusNotFound)
	}
}

type syncBuffer struct {
	b  bytes.Buffer
	mu sync.Mutex
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) lastLine(t *testing.T) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(s.b.String()), "\n")
	if len(lines) == 0 {
		t.Fatal("no log output")
	}
	return lines[len(lines)-1]
}
