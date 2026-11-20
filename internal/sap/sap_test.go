package sap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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

func splitPathFixture(t *testing.T, p string, wantDir, wantName, wantExt string) {
	t.Helper()
	dir, name, ext := splitPath(p)
	if dir != wantDir || name != wantName || ext != wantExt {
		t.Fatalf("splitPath(%q) = (%q,%q,%q), want (%q,%q,%q)", p, dir, name, ext, wantDir, wantName, wantExt)
	}
}

func TestSplitPath(t *testing.T) {
	splitPathFixture(t, `\\192.168.10.248\nas\sales\2d0e8181-9682-4a39-b948-c1b76e1f6040.jpg`,
		`\\192.168.10.248\nas\sales`, "2d0e8181-9682-4a39-b948-c1b76e1f6040", "jpg")
	splitPathFixture(t, "/data/storage/report.PDF", "/data/storage", "report", "PDF")
	splitPathFixture(t, "plainname", ".", "plainname", "")
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
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := New(testConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	err = client.CreateAttachment(context.Background(), `\\192.168.10.248\nas\sales\2d0e8181-9682-4a39-b948-c1b76e1f6040.jpg`)
	if err != nil {
		t.Fatalf("create: %v", err)
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
	if err := client.CreateAttachment(context.Background(), "/data/a.pdf"); err == nil {
		t.Fatal("expected error after failed login")
	}
}

func TestNewDisabled(t *testing.T) {
	client, err := New(&config.Config{})
	if err != nil || client != nil {
		t.Fatalf("client = %v, err = %v; want nil, nil", client, err)
	}
}
