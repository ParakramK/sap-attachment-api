package fetch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/parakram/sap-attachment-api/internal/config"
)

func testConfig(allowPrivate bool, extra func(*config.Config)) *config.Config {
	c := &config.Config{
		S3Region:             "us-east-1",
		FetchTimeout:         5 * 1000 * 1000 * 1000, // 5s
		FetchAllowPrivateNet: allowPrivate,
		S3PathStyle:          true,
		S3AccessKey:          "test-access",
		S3SecretKey:          "test-secret",
	}
	if extra != nil {
		extra(c)
	}
	return c
}

func mustFetcher(t *testing.T, cfg *config.Config) *Fetcher {
	t.Helper()
	f, err := New(cfg)
	if err != nil {
		t.Fatalf("new fetcher: %v", err)
	}
	return f
}

func body(t *testing.T, r *Result) string {
	t.Helper()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = r.Body.Close()
	return string(data)
}

func TestHTTPFetchsPublicOrAllowedPrivate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="report.pdf"`)
		_, _ = w.Write([]byte("hello"))
	}))
	t.Cleanup(srv.Close)

	// httptest binds to loopback: denied by default.
	_, err := mustFetcher(t, testConfig(false, nil)).Fetch(context.Background(), srv.URL)
	if !errors.Is(err, ErrDeniedNetwork) {
		t.Fatalf("err = %v, want ErrDeniedNetwork", err)
	}

	// Allowed: streams content and derives the filename from Content-Disposition.
	res, err := mustFetcher(t, testConfig(true, nil)).Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if body(t, res) != "hello" || res.Filename != "report.pdf" {
		t.Fatalf("result = %q %q", body(t, res), res.Filename)
	}
}

func TestRedirects(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("final"))
	}))
	t.Cleanup(final.Close)

	var hops int
	var mu sync.Mutex
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hops++
		mu.Unlock()
		if hops == 1 {
			http.Redirect(w, r, final.URL, http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(first.Close)

	res, err := mustFetcher(t, testConfig(true, nil)).Fetch(context.Background(), first.URL)
	if err != nil {
		t.Fatalf("redirect fetch: %v", err)
	}
	if body(t, res) != "final" {
		t.Fatalf("body = %q", body(t, res))
	}
}

func TestRedirectToDisallowedScheme(t *testing.T) {
	var hit int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hit++
		mu.Unlock()
		if hit == 1 {
			http.Redirect(w, r, "ftp://example.invalid/file", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("should not be reached"))
	}))
	t.Cleanup(srv.Close)

	_, err := mustFetcher(t, testConfig(true, nil)).Fetch(context.Background(), srv.URL)
	if !errors.Is(err, ErrFetchFailed) {
		t.Fatalf("err = %v, want ErrFetchFailed", err)
	}
}

func TestRedirectLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	_, err := mustFetcher(t, testConfig(true, nil)).Fetch(context.Background(), srv.URL+"/start")
	if !errors.Is(err, ErrFetchFailed) || !strings.Contains(err.Error(), "too many redirects") {
		t.Fatalf("err = %v, want redirect limit", err)
	}
}

func TestURISchemeReject(t *testing.T) {
	for _, uri := range []string{"ftp://example.invalid/x", "file:///etc/passwd", "gopher://x", "data:text/plain,hi"} {
		if _, err := mustFetcher(t, testConfig(true, nil)).Fetch(context.Background(), uri); !errors.Is(err, ErrInvalidURI) {
			t.Errorf("Fetch(%q) err = %v, want ErrInvalidURI", uri, err)
		}
	}
}

func TestHTTPNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	_, err := mustFetcher(t, testConfig(true, nil)).Fetch(context.Background(), srv.URL)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// --- S3 source ---

type fakeS3 struct {
	mu      sync.Mutex
	objects map[string]string // key -> body
	missing []string          // keys that return 404
}

func newFakeS3Source(t *testing.T) (*fakeS3, string) {
	t.Helper()
	f := &fakeS3{objects: map[string]string{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/test-bucket/")
		if key == r.URL.Path || key == "" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			body, ok := f.objects[key]
			missing := false
			for _, m := range f.missing {
				if m == key {
					missing = true
				}
			}
			f.mu.Unlock()
			if !ok || missing {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func TestS3Fetch(t *testing.T) {
	fake, endpoint := newFakeS3Source(t)
	fake.mu.Lock()
	fake.objects["invoices/2026-01.pdf"] = "%PDF-fake"
	fake.mu.Unlock()

	f := mustFetcher(t, testConfig(true, func(c *config.Config) {
		c.S3Endpoint = endpoint
	}))

	res, err := f.Fetch(context.Background(), "s3://test-bucket/invoices/2026-01.pdf")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if body(t, res) != "%PDF-fake" || res.Filename != "2026-01.pdf" || res.Size != int64(len("%PDF-fake")) {
		t.Fatalf("result = %q %q %d", body(t, res), res.Filename, res.Size)
	}
}

func TestS3FetchMissingKey(t *testing.T) {
	_, endpoint := newFakeS3Source(t)
	f := mustFetcher(t, testConfig(true, func(c *config.Config) { c.S3Endpoint = endpoint }))

	_, err := f.Fetch(context.Background(), "s3://test-bucket/nope.pdf")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestS3BucketAllowlist(t *testing.T) {
	_, endpoint := newFakeS3Source(t)
	f := mustFetcher(t, testConfig(true, func(c *config.Config) {
		c.S3Endpoint = endpoint
		c.S3AllowedBuckets = []string{"allowed-bucket"}
	}))

	_, err := f.Fetch(context.Background(), "s3://test-bucket/whatever")
	if !errors.Is(err, ErrBucketDenied) {
		t.Fatalf("err = %v, want ErrBucketDenied", err)
	}
}

func TestS3URIShape(t *testing.T) {
	_, endpoint := newFakeS3Source(t)
	f := mustFetcher(t, testConfig(true, func(c *config.Config) { c.S3Endpoint = endpoint }))

	for _, uri := range []string{"s3://test-bucket", "s3://", "s3://Bad_Bucket/key", "s3://test-bucket/"} {
		if _, err := f.Fetch(context.Background(), uri); !errors.Is(err, ErrInvalidURI) {
			t.Errorf("Fetch(%q) err = %v, want ErrInvalidURI", uri, err)
		}
	}
}

func TestDeniedIPRules(t *testing.T) {
	denied := []string{
		"127.0.0.1", "10.0.0.5", "172.16.1.1", "192.168.0.9", "169.254.169.254",
		"100.64.0.1", "0.0.0.0", "224.0.0.1", "fe80::1", "fd00::1", "::ffff:10.0.0.1", "::",
	}
	for _, ip := range denied {
		if !deniedIP(netip.MustParseAddr(ip)) {
			t.Errorf("deniedIP(%s) = false, want true", ip)
		}
	}
	allowed := []string{"8.8.8.8", "2606:4700::1111", "203.0.113.10"}
	for _, ip := range allowed {
		if deniedIP(netip.MustParseAddr(ip)) {
			t.Errorf("deniedIP(%s) = true, want false", ip)
		}
	}
}
