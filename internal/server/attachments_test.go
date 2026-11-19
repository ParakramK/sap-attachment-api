package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/parakram/sap-attachment-api/internal/config"
	"github.com/parakram/sap-attachment-api/internal/db"
	"github.com/parakram/sap-attachment-api/internal/fetch"
	"github.com/parakram/sap-attachment-api/internal/server"
	"github.com/parakram/sap-attachment-api/internal/service"
	"github.com/parakram/sap-attachment-api/internal/storage"
)

type apiFixture struct {
	app   *fiber.App
	store *storage.Local
	dir   string
}

// newAPI builds a fully wired app against temp dirs. Keys configures API
// keys (nil disables auth); extra mutates the config before wiring.
func newAPI(t *testing.T, keys []string, extra func(*config.Config)) *apiFixture {
	t.Helper()
	cfg := testConfig()
	cfg.DBPath = filepath.Join(t.TempDir(), "test.db") // per-test DB: dedupe must not see earlier runs
	if keys != nil {
		cfg.Env = "prod"
		cfg.APIKeys = keys
	}
	if extra != nil {
		extra(cfg)
	}

	d, err := db.Open(cfg)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	repo := db.NewRepo(d)

	dir := t.TempDir()
	store, err := storage.NewLocal(filepath.Join(dir, "storage"))
	if err != nil {
		t.Fatal(err)
	}
	uploader, err := service.NewUploader(repo, store, filepath.Join(dir, "staging"), cfg.MaxUploadBytes())
	if err != nil {
		t.Fatal(err)
	}
	fetcher, err := fetch.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	app := server.New(server.Deps{
		Cfg:      cfg,
		Log:      testLogger(),
		Ready:    func(context.Context) error { return nil },
		Repo:     repo,
		Store:    store,
		Uploader: uploader,
		Fetcher:  fetcher,
	})
	return &apiFixture{app: app, store: store, dir: dir}
}

func doJSON(t *testing.T, app *fiber.App, method, path, body, apiKey string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Idempotency-Key", newIdemKey())
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// newIdemKey returns a fresh 36-char key: each request in tests must use a
// different one, or the middleware replays the first response.
var idemSeq atomic.Uint64

func newIdemKey() string {
	return fmt.Sprintf("11111111-2222-4333-8444-%012d", idemSeq.Add(1))
}

func multipartBody(t *testing.T, field, filename, content string) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String(), mw.FormDataContentType()
}

func doMultipart(t *testing.T, app *fiber.App, path, filename, content, apiKey string) *http.Response {
	return doMultipartWithKey(t, app, path, filename, content, apiKey, newIdemKey())
}

func doMultipartWithKey(t *testing.T, app *fiber.App, path, filename, content, apiKey, idemKey string) *http.Response {
	t.Helper()
	body, ct := multipartBody(t, "file", filename, content)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Idempotency-Key", idemKey)
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func TestCreateAndGetAttachment(t *testing.T) {
	f := newAPI(t, nil, nil)

	key := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	resp := doMultipartWithKey(t, f.app, "/v1/attachments", "invoice.pdf", "%PDF-1.4 content", "", key)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	created := decode[attachmentShape](t, resp)
	if created.Filename != key+".pdf" || created.Status != "available" {
		t.Fatalf("created = %+v", created)
	}
	if filepath.Base(created.StoragePath) != created.Filename {
		t.Fatalf("stored name %q != filename %q", created.StoragePath, created.Filename)
	}
	if created.StoragePath == "" || !filepath.IsAbs(created.StoragePath) {
		t.Fatalf("storage_path = %q", created.StoragePath)
	}
	if _, err := os.Stat(created.StoragePath); err != nil {
		t.Fatalf("stored object missing: %v", err)
	}

	resp = doJSON(t, f.app, http.MethodGet, "/v1/attachments/"+created.ID, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d", resp.StatusCode)
	}
	got := decode[attachmentShape](t, resp)
	if got.ID != created.ID || got.SHA256 != created.SHA256 {
		t.Fatalf("GET = %+v", got)
	}
}

type attachmentShape struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	SHA256      string `json:"sha256"`
	Status      string `json:"status"`
	StoragePath string `json:"storage_path"`
}

func TestCreateDedupeReturnsExisting(t *testing.T) {
	f := newAPI(t, nil, nil)

	firstKey := "11111111-2222-4333-8444-555555555555"
	first := doMultipartWithKey(t, f.app, "/v1/attachments", "a.txt", "same-bytes", "", firstKey)
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d", first.StatusCode)
	}
	a := decode[attachmentShape](t, first)

	second := doMultipartWithKey(t, f.app, "/v1/attachments", "b.txt", "same-bytes", "", newIdemKey())
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, want 200 (deduped)", second.StatusCode)
	}
	b := decode[attachmentShape](t, second)
	if b.ID != a.ID || b.Filename != firstKey+".txt" {
		t.Fatalf("dedupe returned %+v", b)
	}
}

func TestFilenameIsKeyPlusOriginalExtension(t *testing.T) {
	f := newAPI(t, nil, nil)
	key := "99999999-8888-4777-8666-555555555555"
	// A client filename far too long for Windows paths: only its extension
	// may survive, and it is lowercased/bounded by storage.Ext.
	longName := strings.Repeat("a-very-long-customer-filename-", 20) + ".PDF"

	resp := doMultipartWithKey(t, f.app, "/v1/attachments", longName, "%PDF-1.4 content", "", key)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	created := decode[attachmentShape](t, resp)
	if created.Filename != key+".pdf" {
		t.Fatalf("filename = %q, want %q", created.Filename, key+".pdf")
	}
	if len(created.Filename) > 60 {
		t.Fatalf("filename still too long: %q", created.Filename)
	}
	if filepath.Base(created.StoragePath) != created.Filename {
		t.Fatalf("stored name = %q, want %q", created.StoragePath, created.Filename)
	}
}

func TestCreateFromHTTPSource(t *testing.T) {
	f := newAPI(t, nil, nil)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("downloaded content"))
	}))
	t.Cleanup(upstream.Close)

	resp := doJSON(t, f.app, http.MethodPost, "/v1/attachments",
		fmt.Sprintf(`{"source_url": %q}`, upstream.URL), "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	created := decode[attachmentShape](t, resp)
	if created.Filename == "" || created.SizeBytes != int64(len("downloaded content")) {
		t.Fatalf("created = %+v", created)
	}
}

func TestCreateFromS3Source(t *testing.T) {
	var mu sync.Mutex
	objects := map[string]string{"docs/file.txt": "s3 body"}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/src-bucket/")
		mu.Lock()
		body, ok := objects[key]
		mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	s3Fake := httptest.NewServer(mux)
	t.Cleanup(s3Fake.Close)

	f := newAPI(t, nil, func(c *config.Config) {
		c.S3Endpoint = s3Fake.URL
		c.S3PathStyle = true
		c.S3AccessKey = "test"
		c.S3SecretKey = "test"
	})

	resp := doJSON(t, f.app, http.MethodPost, "/v1/attachments",
		`{"source_uri": "s3://src-bucket/docs/file.txt"}`, "")
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	// JSON requests use doJSON's generated key; just check the extension came
	// from the remote filename (the key is asserted in TestFilenameIsKey...).
	created := decode[attachmentShape](t, resp)
	if !strings.HasSuffix(created.Filename, ".txt") || strings.Contains(created.Filename, "file") {
		t.Fatalf("created = %+v", created)
	}
}

func TestCreateRejectsBadSources(t *testing.T) {
	f := newAPI(t, nil, nil)

	cases := []struct {
		name, body string
		want       int
	}{
		{"no source", `{}`, http.StatusBadRequest},
		{"both sources", `{"source_url":"https://a","source_uri":"s3://b/k"}`, http.StatusBadRequest},
		{"bad scheme", `{"source_url": "ftp://example.invalid/x"}`, http.StatusBadRequest},
		{"bad s3 shape", `{"source_uri": "s3://bucket"}`, http.StatusBadRequest},
		{"bad json", `{source_url:`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doJSON(t, f.app, http.MethodPost, "/v1/attachments", tc.body, "")
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestCreateOversizedUpload(t *testing.T) {
	f := newAPI(t, nil, nil)
	big := strings.Repeat("x", 11<<20) // fixture cap is 10MB

	resp := doMultipart(t, f.app, "/v1/attachments", "big.bin", big, "")
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestAuthRequired(t *testing.T) {
	f := newAPI(t, []string{"secret-key-1"}, nil)

	resp := doMultipart(t, f.app, "/v1/attachments", "a.txt", "x", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	resp = doMultipart(t, f.app, "/v1/attachments", "a.txt", "x", "wrong-key")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	resp = doMultipart(t, f.app, "/v1/attachments", "a.txt", "x", "secret-key-1")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
}

func TestIdempotencyReplaysResponse(t *testing.T) {
	f := newAPI(t, nil, nil)

	body, ct := multipartBody(t, "file", "same.txt", "idempotent content")
	send := func() *http.Response {
		req := httptest.NewRequest(http.MethodPost, "/v1/attachments", strings.NewReader(body))
		req.Header.Set("Content-Type", ct)
		req.Header.Set("X-Idempotency-Key", "11111111-2222-4333-8444-555555555555")
		resp, err := f.app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	first := send()
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d", first.StatusCode)
	}
	a := decode[attachmentShape](t, first)

	second := send()
	if second.StatusCode != http.StatusCreated {
		t.Fatalf("replayed status = %d", second.StatusCode)
	}
	b := decode[attachmentShape](t, second)
	if b.ID != a.ID {
		t.Fatalf("replay returned different attachment: %s vs %s", b.ID, a.ID)
	}
}

func TestIdempotencyKeyRequired(t *testing.T) {
	f := newAPI(t, nil, nil)
	body, ct := multipartBody(t, "file", "a.txt", "key required")

	send := func(key string) *http.Response {
		req := httptest.NewRequest(http.MethodPost, "/v1/attachments", strings.NewReader(body))
		req.Header.Set("Content-Type", ct)
		if key != "" {
			req.Header.Set("X-Idempotency-Key", key)
		}
		resp, err := f.app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	for _, tc := range []struct {
		name, key string
		want      int
	}{
		{"missing", "", http.StatusBadRequest},
		{"too short", "short-key", http.StatusBadRequest},
		{"valid uuid", "11111111-2222-4333-8444-555555555555", http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := send(tc.key).StatusCode; got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDeleteAttachment(t *testing.T) {
	f := newAPI(t, nil, nil)
	created := decode[attachmentShape](t, doMultipart(t, f.app, "/v1/attachments", "gone.pdf", "%PDF-1.4 gone", ""))
	path := created.StoragePath

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("object missing before delete: %v", err)
	}

	resp := doJSON(t, f.app, http.MethodDelete, "/v1/attachments/"+created.ID, "", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("stored object not removed")
	}

	resp = doJSON(t, f.app, http.MethodGet, "/v1/attachments/"+created.ID, "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete = %d", resp.StatusCode)
	}

	// Deleting again is a clean 404.
	resp = doJSON(t, f.app, http.MethodDelete, "/v1/attachments/"+created.ID, "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("re-delete = %d", resp.StatusCode)
	}
}
