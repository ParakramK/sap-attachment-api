package server_test

import (
	"bytes"
	"encoding/json"
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
		Env:             "dev",
		Addr:            ":0",
		LogLevel:        "info",
		StorageDir:      "./data/storage",
		DBPath:          "./data/test.db",
		MaxUploadMB:     10,
		ShutdownTimeout: time.Second,
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
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

func TestHealthz(t *testing.T) {
	app := server.New(testConfig(), testLogger())
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
	app := server.New(testConfig(), testLogger())
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
	app := server.New(testConfig(), log)

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
