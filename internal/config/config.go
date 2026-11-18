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
	"strings"
	"time"

	"github.com/ilyakaznacheev/cleanenv"
)

type Config struct {
	Env  string `env:"APP_ENV" env-default:"dev"` // dev | prod
	Addr string `env:"ADDR" env-default:":8080"`

	LogLevel     string `env:"LOG_LEVEL" env-default:"info"`
	LogtailToken string `env:"LOGTAIL_TOKEN"` // Better Stack source token (secret)
	LogtailLevel string `env:"LOGTAIL_LEVEL" env-default:"info"`

	StorageDir string `env:"STORAGE_DIR" env-default:"./data/storage"`
	DBPath     string `env:"DB_PATH" env-default:"./data/attachments.db"`

	APIKeys     []string `env:"API_KEYS" env-separator:","`
	MaxUploadMB int      `env:"MAX_UPLOAD_MB" env-default:"50"`

	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" env-default:"15s"`
}

func Load() (*Config, error) {
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
	if c.StorageDir == "" || c.DBPath == "" {
		return errors.New("STORAGE_DIR and DB_PATH must not be empty")
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
