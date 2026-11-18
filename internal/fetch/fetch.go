// Package fetch downloads remote sources (http(s) URLs and s3:// URIs) for
// ingestion, with SSRF defenses: connections are validated against the actual
// dialed IP (DNS-rebinding safe), redirects are bounded and re-validated, and
// every scheme other than http/https/s3 is rejected.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/parakram/sap-attachment-api/internal/config"
)

// Typed errors the API layer maps to distinct HTTP responses.
var (
	ErrInvalidURI    = errors.New("invalid source URI")
	ErrBucketDenied  = errors.New("s3 bucket not allowed")
	ErrNotFound      = errors.New("remote source not found")
	ErrDeniedNetwork = errors.New("remote host resolves to a denied network")
	ErrFetchFailed   = errors.New("remote fetch failed")
)

const maxRedirects = 3

// Result is a successfully opened remote source.
type Result struct {
	// Body is the content stream; Close releases the connection.
	Body io.ReadCloser
	// Filename is the best-effort file name derived from the source.
	Filename string
	// Size is the source-declared size, or -1 when unknown.
	Size int64
}

// Fetcher downloads remote sources for ingestion.
type Fetcher struct {
	client *http.Client
	s3     *s3.Client

	allowedBuckets map[string]struct{} // nil = any bucket
	timeout        time.Duration
}

func New(cfg *config.Config) (*Fetcher, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// A configured proxy would dial on our behalf and bypass IP validation.
	transport.Proxy = nil
	transport.DialContext = hardenedDialContext(cfg.FetchAllowPrivateNet)

	client := &http.Client{
		Transport: transport,
		Timeout:   cfg.FetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("%w: too many redirects", ErrFetchFailed)
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("%w: redirect to %q scheme", ErrFetchFailed, req.URL.Scheme)
			}
			return nil
		},
	}

	s3Client, err := newS3Client(cfg)
	if err != nil {
		return nil, err
	}

	var allowed map[string]struct{}
	if len(cfg.S3AllowedBuckets) > 0 {
		allowed = make(map[string]struct{}, len(cfg.S3AllowedBuckets))
		for _, b := range cfg.S3AllowedBuckets {
			allowed[strings.ToLower(b)] = struct{}{}
		}
	}

	return &Fetcher{
		client:         client,
		s3:             s3Client,
		allowedBuckets: allowed,
		timeout:        cfg.FetchTimeout,
	}, nil
}

// Fetch opens the remote source and returns its content stream.
func (f *Fetcher) Fetch(ctx context.Context, raw string) (*Result, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidURI, err)
	}

	switch u.Scheme {
	case "http", "https":
		return f.fetchHTTP(ctx, u)
	case "s3":
		return f.fetchS3(ctx, u)
	default:
		return nil, fmt.Errorf("%w: unsupported scheme %q", ErrInvalidURI, u.Scheme)
	}
}

// fetchHTTP streams an http(s) URL. The dial-time IP validation makes every
// hop (including redirects) DNS-rebinding safe; CheckRedirect bounds hops.
func (f *Fetcher) fetchHTTP(ctx context.Context, u *url.URL) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, f.timeout)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: %v", ErrInvalidURI, err)
	}
	req.Header.Set("User-Agent", "sap-attachment-api")

	resp, err := f.client.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: %w", ErrFetchFailed, err)
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		cancel()
		if resp.StatusCode == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("%w: upstream status %s", ErrFetchFailed, strconv.Itoa(resp.StatusCode))
	}

	return &Result{
		Body:     bodyCloser{Reader: resp.Body, close: func() error { cancel(); return nil }},
		Filename: httpFilename(resp.Header.Get("Content-Disposition"), u.Path),
		Size:     resp.ContentLength, // -1 when unknown
	}, nil
}

// bodyCloser runs extra teardown (e.g. context cancel) on Close.
type bodyCloser struct {
	io.Reader
	close func() error
}

func (b bodyCloser) Close() error { return b.close() }

// hardenedDialContext returns a dialer whose Control callback validates the
// exact IP each connection uses — the rebinding-safe check point.
func hardenedDialContext(allowPrivate bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if !allowPrivate {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("%w: bad address %q", ErrDeniedNetwork, addr)
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return nil, fmt.Errorf("%w: unparseable address %q", ErrDeniedNetwork, addr)
			}
			if deniedIP(ip) {
				return nil, fmt.Errorf("%w: %s", ErrDeniedNetwork, ip)
			}
		}
		return dialer.DialContext(ctx, network, addr)
	}
}

// deniedIP reports whether the address is loopback, private, link-local,
// multicast, unspecified, CGNAT, or a non-IP (all denied by default).
func deniedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if cgnat.Contains(ip) {
		return true
	}
	return false
}

// cgnat is RFC 6598 shared address space (some cloud metadata endpoints).
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// httpFilename prefers a validated Content-Disposition filename, else the URL
// path base, else empty.
func httpFilename(contentDisposition, urlPath string) string {
	if contentDisposition != "" {
		_, params, err := mime.ParseMediaType(contentDisposition)
		if err == nil {
			if fn := params["filename"]; fn != "" && safeFilename(fn) {
				return path.Base(fn)
			}
		}
	}
	if base := path.Base(urlPath); base != "/" && base != "." && safeFilename(base) {
		return base
	}
	return ""
}

// safeFilename rejects names that could smuggle path separators or control
// characters. The caller sanitizes further before storing.
func safeFilename(name string) bool {
	if strings.ContainsAny(name, "/\\\x00") {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return strings.TrimSpace(name) != ""
}

func newS3Client(cfg *config.Config) (*s3.Client, error) {
	awscfg, err := awsconfig.LoadDefaultConfig(
		context.Background(),
		awsconfig.WithRegion(cfg.S3Region),
	)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	// Explicit keys win; otherwise fall back to the AWS default credential
	// chain (env, shared config, IAM role).
	if cfg.S3AccessKey != "" {
		awscfg.Credentials = credentials.NewStaticCredentialsProvider(
			cfg.S3AccessKey, cfg.S3SecretKey, "")
	}

	return s3.NewFromConfig(awscfg, func(o *s3.Options) {
		if cfg.S3Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.S3Endpoint)
		}
		o.UsePathStyle = cfg.S3PathStyle
	}), nil
}

// fetchS3 streams an s3://bucket/key object.
func (f *Fetcher) fetchS3(ctx context.Context, u *url.URL) (*Result, error) {
	bucket := strings.ToLower(u.Host)
	key := strings.TrimPrefix(u.Path, "/")

	if bucket == "" || key == "" || u.Path == "" {
		return nil, fmt.Errorf("%w: s3 URI must be s3://bucket/key", ErrInvalidURI)
	}
	if !validBucketName(bucket) {
		return nil, fmt.Errorf("%w: invalid bucket name %q", ErrInvalidURI, bucket)
	}
	if f.allowedBuckets != nil {
		if _, ok := f.allowedBuckets[bucket]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrBucketDenied, bucket)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	out, err := f.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		cancel()
		if isS3NotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("%w: s3 get: %w", ErrFetchFailed, err)
	}

	return &Result{
		Body:     bodyCloser{Reader: out.Body, close: func() error { cancel(); return out.Body.Close() }},
		Filename: path.Base(key),
		Size:     aws.ToInt64(out.ContentLength),
	}, nil
}

// validBucketName checks the classic S3 bucket name rules loosely enough for
// allowlist-style validation.
func validBucketName(bucket string) bool {
	if len(bucket) < 3 || len(bucket) > 63 {
		return false
	}
	for _, r := range bucket {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
		default:
			return false
		}
	}
	return !strings.HasPrefix(bucket, "-") && !strings.HasSuffix(bucket, "-") &&
		!strings.HasPrefix(bucket, ".") && !strings.HasSuffix(bucket, ".")
}

// isS3NotFound recognizes missing keys across the SDK error types and
// S3-compatible implementations that omit the canonical error body.
func isS3NotFound(err error) bool {
	var nk *types.NoSuchKey
	if errors.As(err, &nk) {
		return true
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var re interface{ HTTPStatusCode() int }
	if errors.As(err, &re) && re.HTTPStatusCode() == 404 {
		return true
	}
	return false
}
