package config

import (
	"testing"
	"time"

	"log/slog"
)

func validConfig() Config {
	return Config{
		Env:             "dev",
		Addr:            ":8080",
		LogLevel:        "info",
		LogtailLevel:    "info",
		StorageDir:      "./data/storage",
		StagingDir:      "./data/staging",
		DBPath:          "./data/attachments.db",
		S3Region:        "us-east-1",
		MaxUploadMB:     50,
		ShutdownTimeout: 15 * time.Second,
	}
}

func TestValidateAcceptsValid(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateProdRequiresAPIKeys(t *testing.T) {
	c := validConfig()
	c.Env = "prod"
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for prod without API_KEYS")
	}

	c.APIKeys = []string{"secret"}
	if err := c.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := map[string]func(*Config){
		"env":      func(c *Config) { c.Env = "staging" },
		"addr":     func(c *Config) { c.Addr = " " },
		"loglevel": func(c *Config) { c.LogLevel = "verbose" },
		"storage":  func(c *Config) { c.StorageDir = "" },
		"upload":   func(c *Config) { c.MaxUploadMB = 0 },
		"timeout":  func(c *Config) { c.ShutdownTimeout = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatalf("%s: expected error", name)
			}
		})
	}
}

func TestValidateS3CredentialsRules(t *testing.T) {
	c := validConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("s3 fetch config optional: %v", err)
	}

	c.S3AccessKey = "AKIA..."
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for access key without secret")
	}

	c.S3SecretKey = "secret"
	if err := c.Validate(); err != nil {
		t.Fatalf("s3 with static creds should pass: %v", err)
	}
}

func TestLevelParsing(t *testing.T) {
	c := validConfig()
	c.LogLevel = "debug"
	c.LogtailLevel = "WARN"
	if got := c.Level(); got != slog.LevelDebug {
		t.Fatalf("Level() = %v, want DEBUG", got)
	}
	if got := c.RemoteLevel(); got != slog.LevelWarn {
		t.Fatalf("LogtailLevel() = %v, want WARN", got)
	}
	if got := c.MaxUploadBytes(); got != 50<<20 {
		t.Fatalf("MaxUploadBytes() = %d, want %d", got, 50<<20)
	}
}
