package sap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/parakram/sap-attachment-api/internal/config"
)

func testConfig(url string) *config.Config {
	return &config.Config{
		SAPDatabase:        "SBODEMO",
		SAPServiceLayerURL: url,
		SAPUsername:        "manager",
		SAPPassword:        "secret",
	}
}

func TestCreateAttachment(t *testing.T) {
	var loginBody map[string]string
	var attachmentBody map[string]any
	var gotCookie string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/b1s/v1/Login":
			if err := json.NewDecoder(r.Body).Decode(&loginBody); err != nil {
				t.Errorf("login decode: %v", err)
			}
			// Test-only cookie; transport security is irrelevant on httptest.
			http.SetCookie(w, &http.Cookie{Name: "SESSIONID", Value: "session-123", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
			w.WriteHeader(http.StatusOK)
		case "/b1s/v1/Attachments2":
			if c, err := r.Cookie("SESSIONID"); err != nil {
				t.Errorf("session cookie missing: %v", err)
			} else {
				gotCookie = c.Value
			}
			if err := json.NewDecoder(r.Body).Decode(&attachmentBody); err != nil {
				t.Errorf("attachment decode: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"AbsoluteEntry":148076,"Attachments2_Lines":[{"SourcePath":"x"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := New(testConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	path := `\\192.168.10.248\nas\sales\2d0e8181-9682-4a39-b948-c1b76e1f6040.jpg`
	entry, err := client.CreateAttachment(context.Background(), path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if entry != 148076 {
		t.Fatalf("AbsoluteEntry = %d, want 148076", entry)
	}

	if loginBody["CompanyDatabase"] != "SBODEMO" || loginBody["UserName"] != "manager" || loginBody["Password"] != "secret" {
		t.Fatalf("login body = %+v", loginBody)
	}
	if gotCookie != "session-123" {
		t.Fatalf("session cookie = %q", gotCookie)
	}
	lines, ok := attachmentBody["Attachments2_Lines"].([]any)
	if !ok || len(lines) != 1 {
		t.Fatalf("lines = %+v", attachmentBody)
	}
	line := lines[0].(map[string]any)
	if line["SourcePath"] != `\\192.168.10.248\nas\sales` ||
		line["FileName"] != "2d0e8181-9682-4a39-b948-c1b76e1f6040" ||
		line["FileExtension"] != "jpg" {
		t.Fatalf("line = %+v", line)
	}
	if _, exists := line["UserID"]; exists {
		t.Fatal("UserID must not be sent")
	}
}

func TestCreateAttachmentLoginFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/b1s/v1/Login" {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		t.Error("Attachments2 must not be called after a failed login")
	}))
	t.Cleanup(srv.Close)

	client, err := New(testConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateAttachment(context.Background(), "/data/a.pdf"); err == nil {
		t.Fatal("expected error after failed login")
	}
}

func TestSessionReuseAndWarmRefresh(t *testing.T) {
	old := refreshInterval
	refreshInterval = 100 * time.Millisecond
	t.Cleanup(func() { refreshInterval = old })

	var logins atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/b1s/v1/Login":
			logins.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "SESSIONID", Value: "s", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		case "/b1s/v1/Attachments2":
			_, _ = w.Write([]byte(`{"AbsoluteEntry":42}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := New(testConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { client.Start(ctx); close(done) }()

	// Burst of uploads reuses one login.
	for range 2 {
		if _, err := client.CreateAttachment(ctx, "/data/a.pdf"); err != nil {
			t.Fatal(err)
		}
	}
	if got := logins.Load(); got != 1 {
		t.Fatalf("logins after two uploads = %d, want 1", got)
	}

	// The background loop refreshes without any uploads.
	deadline := time.Now().Add(3 * time.Second)
	for logins.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if logins.Load() < 2 {
		t.Fatalf("session was not refreshed (logins=%d)", logins.Load())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Start did not stop on context cancellation")
	}
	count := logins.Load()
	time.Sleep(250 * time.Millisecond)
	if logins.Load() != count {
		t.Fatal("refresh continued after cancellation")
	}
}

func TestNewDisabled(t *testing.T) {
	client, err := New(&config.Config{})
	if err != nil || client != nil {
		t.Fatalf("client = %v, err = %v; want nil, nil", client, err)
	}
}
