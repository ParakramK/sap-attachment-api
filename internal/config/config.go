// Package config loads and validates service configuration.
//
// Configuration is environment-first (12-factor). Secrets (API keys, remote
// logging token) are only read from the environment — never from files inside
// the repository. If a deployment needs file-based secrets (e.g. Docker
// secrets), the entrypoint can `export` them from /run/secrets before start.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/ilyakaznacheev/cleanenv"
	"github.com/joho/godotenv"
)

type Config struct {
	Env  string `env:"APP_ENV" env-default:"dev"` // dev | prod
	Addr string `env:"ADDR" env-default:":8080"`

	LogLevel     string `env:"LOG_LEVEL" env-default:"info"`
	LogtailToken string `env:"LOGTAIL_TOKEN"` // Better Stack source token (secret)
	LogtailLevel string `env:"LOGTAIL_LEVEL" env-default:"info"`
	// SQL_DEBUG dumps every SQL statement (bundebug). Off by default: even
	// normal misses like "no rows" would otherwise flood the logs.
	SQLDebug bool `env:"SQL_DEBUG" env-default:"false"`

	StorageDir string `env:"STORAGE_DIR" env-default:"./data/storage"`
	StagingDir string `env:"STAGING_DIR" env-default:"./data/staging"` // scratch space; always local
	DBPath     string `env:"DB_PATH" env-default:"./data/attachments.db"`

	// Storage backend: local filesystem, or a remote SMB2/3 share the service
	// speaks to directly (no OS mount needed).
	StorageBackend string `env:"STORAGE_BACKEND" env-default:"local"` // local | smb

	// SMB backend settings (used when STORAGE_BACKEND=smb).
	SMBServer   string `env:"SMB_SERVER"` // host or host:port (default port 445)
	SMBShare    string `env:"SMB_SHARE"`
	SMBUsername string `env:"SMB_USERNAME"`
	SMBPassword string `env:"SMB_PASSWORD"` // secret
	SMBDomain   string `env:"SMB_DOMAIN"`   // optional AD domain
	// Require SMB3 message signing (integrity); disable only for legacy NAS.
	SMBRequireSigning bool `env:"SMB_REQUIRE_SIGNING" env-default:"true"`
	// Optional subfolder inside the share where all objects live, e.g.
	// "attachments" for \\server\nas\attachments. Empty = share root.
	SMBBaseDir string `env:"SMB_BASE_DIR"`
	// Where commits are staged inside the share before the atomic rename.
	SMBStagingDir string `env:"SMB_STAGING_DIR" env-default:"staging"`

	// Remote-source fetcher settings: used when ingesting from S3 URIs.
	// Credentials fall back to the AWS default credential chain (IAM role).
	S3Region    string `env:"S3_REGION" env-default:"us-east-1"`
	S3Endpoint  string `env:"S3_ENDPOINT"` // custom endpoint (e.g. MinIO); empty = AWS
	S3PathStyle bool   `env:"S3_PATH_STYLE" env-default:"false"`
	S3AccessKey string `env:"S3_ACCESS_KEY_ID"`
	S3SecretKey string `env:"S3_SECRET_ACCESS_KEY"` // secret

	APIKeys     []string `env:"API_KEYS" env-separator:","`
	MaxUploadMB int      `env:"MAX_UPLOAD_MB" env-default:"50"`

	// Remote-source fetcher: rejects connections to private/reserved networks
	// (SSRF defense). Enable only for deployments whose remote sources are
	// themselves on private networks.
	FetchAllowPrivateNet bool `env:"FETCH_ALLOW_PRIVATE_NET" env-default:"false"`
	// Optional allowlist for s3:// sources; empty = any bucket the
	// credentials can read.
	S3AllowedBuckets []string      `env:"S3_ALLOWED_BUCKETS" env-separator:","`
	FetchTimeout     time.Duration `env:"FETCH_TIMEOUT" env-default:"60s"`

	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" env-default:"15s"`
}

// Load reads config from the process environment, an explicit dotenv file,
// or ./.env (in that order of precedence). Nothing is baked into the binary:
// every host supplies its own values at startup.
func Load() (*Config, error) { return LoadFrom("") }

// LoadFrom loads configuration, optionally from an explicit dotenv path.
// A path from CONFIG_FILE is used when none is passed. Real environment
// variables always win over values in a file.
func LoadFrom(path string) (*Config, error) {
	if path == "" {
		path = os.Getenv("CONFIG_FILE")
	}
	if path != "" {
		// Explicit path: a missing file is an error, not a silent fallback.
		if err := godotenv.Load(path); err != nil {
			return nil, fmt.Errorf("load config file %q: %w", path, err)
		}
	} else if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("load .env: %w", err)
	}
	var c Config
	if err := cleanenv.ReadEnv(&c); err != nil {
		return nil, fmt.Errorf("read environment: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	return &c, nil
}

func (c Config) Validate() error {
	switch c.Env {
	case "dev", "prod":
	default:
		return fmt.Errorf("APP_ENV must be dev or prod, got %q", c.Env)
	}
	if strings.TrimSpace(c.Addr) == "" {
		return errors.New("ADDR must not be empty")
	}
	if _, err := parseLevel(c.LogLevel); err != nil {
		return fmt.Errorf("LOG_LEVEL: %w", err)
	}
	if _, err := parseLevel(c.LogtailLevel); err != nil {
		return fmt.Errorf("LOGTAIL_LEVEL: %w", err)
	}
	if c.StorageDir == "" || c.DBPath == "" || c.StagingDir == "" {
		return errors.New("STORAGE_DIR, STAGING_DIR and DB_PATH must not be empty")
	}
	// SQLite needs local, coherent file locking: never run the DB on SMB/NFS.
	// Only attachments may live on the SMB share.
	if strings.HasPrefix(c.DBPath, `\\`) || strings.HasPrefix(c.DBPath, "//") {
		return errors.New("DB_PATH must be local; SQLite cannot run on SMB/NFS shares")
	}
	switch c.StorageBackend {
	case "local":
	case "smb":
		if c.SMBServer == "" || c.SMBShare == "" || c.SMBUsername == "" || c.SMBPassword == "" {
			return errors.New("STORAGE_BACKEND=smb requires SMB_SERVER, SMB_SHARE, SMB_USERNAME and SMB_PASSWORD")
		}
		if c.SMBStagingDir == "" {
			return errors.New("SMB_STAGING_DIR must not be empty")
		}
		if strings.HasPrefix(c.SMBStagingDir, "/") || strings.Contains(c.SMBStagingDir, `\`) ||
			strings.Contains(c.SMBStagingDir, "..") {
			return errors.New("SMB_STAGING_DIR must be a share-relative path without .. or backslashes")
		}
		if c.SMBBaseDir != "" && (strings.HasPrefix(c.SMBBaseDir, "/") ||
			strings.Contains(c.SMBBaseDir, `\`) || strings.Contains(c.SMBBaseDir, "..")) {
			return errors.New("SMB_BASE_DIR must be a share-relative path without .. or backslashes")
		}
	default:
		return fmt.Errorf("STORAGE_BACKEND must be local or smb, got %q", c.StorageBackend)
	}
	if (c.S3AccessKey == "") != (c.S3SecretKey == "") {
		return errors.New("S3_ACCESS_KEY_ID and S3_SECRET_ACCESS_KEY must be set together (or both omitted to use the AWS default credential chain)")
	}
	if c.S3Region == "" {
		return errors.New("S3_REGION must not be empty")
	}
	if c.Env == "prod" && len(c.APIKeys) == 0 {
		return errors.New("API_KEYS is required when APP_ENV=prod")
	}
	if c.MaxUploadMB <= 0 {
		return errors.New("MAX_UPLOAD_MB must be positive")
	}
	if c.ShutdownTimeout <= 0 {
		return errors.New("SHUTDOWN_TIMEOUT must be positive")
	}
	if c.FetchTimeout <= 0 {
		return errors.New("FETCH_TIMEOUT must be positive")
	}
	return nil
}

func (c *Config) Level() slog.Level       { level, _ := parseLevel(c.LogLevel); return level }
func (c *Config) RemoteLevel() slog.Level { level, _ := parseLevel(c.LogtailLevel); return level }
func (c *Config) MaxUploadBytes() int64   { return int64(c.MaxUploadMB) << 20 }

func parseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.TrimSpace(s))); err != nil {
		return 0, err
	}
	return l, nil
}
