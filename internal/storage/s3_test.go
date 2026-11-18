package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/parakram/sap-attachment-api/internal/config"
)

// fakeS3 implements just enough S3 wire behavior (PUT/HEAD/DELETE on
// /<bucket>/<key>, path-style) to exercise the backend end-to-end.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string]string // key -> body
	types   map[string]string // key -> content type
}

func newFakeS3(t *testing.T) (*fakeS3, string) {
	t.Helper()
	f := &fakeS3{objects: map[string]string{}, types: map[string]string{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/test-bucket/")
		if key == r.URL.Path || key == "" {
			http.NotFound(w, r)
			return
		}

		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.objects[key] = string(body)
			f.types[key] = r.Header.Get("Content-Type")
			f.mu.Unlock()
			w.Header().Set("ETag", `"fake-etag"`)
			w.WriteHeader(http.StatusOK)
		case http.MethodHead:
			f.mu.Lock()
			_, ok := f.objects[key]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(f.objects[key])))
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			f.mu.Lock()
			delete(f.objects, key)
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func newS3Backend(t *testing.T, endpoint string) *S3 {
	t.Helper()
	b, err := NewS3(&config.Config{
		StorageBackend: "s3",
		S3Bucket:       "test-bucket",
		S3Region:       "us-east-1",
		S3Endpoint:     endpoint,
		S3PathStyle:    true,
		S3AccessKey:    "test-access",
		S3SecretKey:    "test-secret",
	})
	if err != nil {
		t.Fatalf("new s3 backend: %v", err)
	}
	return b
}

func TestS3SaveHasDelete(t *testing.T) {
	fake, endpoint := newFakeS3(t)
	backend := newS3Backend(t, endpoint)
	ctx := context.Background()

	staged := stagedContent(t, t.TempDir(), "s3 object body")
	rel, err := backend.Save(ctx, staged, "scan.png", "image/png")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if rel != staged.SHA256[:2]+"/"+staged.SHA256+".png" {
		t.Fatalf("rel = %q", rel)
	}

	fake.mu.Lock()
	body, ok := fake.objects[rel]
	ct := fake.types[rel]
	fake.mu.Unlock()
	if !ok || body != "s3 object body" || ct != "image/png" {
		t.Fatalf("stored wrong: %q %q", body, ct)
	}
	if got := backend.Reference(rel); got != "s3://test-bucket/"+rel {
		t.Fatalf("reference = %q", got)
	}

	if has, err := backend.Has(ctx, rel); err != nil || !has {
		t.Fatalf("has after save: %v %v", err, has)
	}
	if has, err := backend.Has(ctx, "missing.png"); err != nil || has {
		t.Fatalf("has missing: %v %v", err, has)
	}

	if err := backend.Delete(ctx, rel); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if has, err := backend.Has(ctx, rel); err != nil || has {
		t.Fatalf("has after delete: %v %v", err, has)
	}
	// Delete is idempotent.
	if err := backend.Delete(ctx, rel); err != nil {
		t.Fatalf("re-delete: %v", err)
	}
}

func TestS3HasTreatsBare404AsMissing(t *testing.T) {
	_, endpoint := newFakeS3(t)
	backend := newS3Backend(t, endpoint)

	// Some S3-compatible stores return bare 404s without the canonical
	// NotFound error body; the fake always does, and Has must cope.
	has, err := backend.Has(context.Background(), "definitely-missing")
	if err != nil {
		t.Fatalf("has missing: %v", err)
	}
	if has {
		t.Fatal("missing object reported present")
	}
}
