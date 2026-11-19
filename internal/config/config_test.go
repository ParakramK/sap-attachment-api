package config

import (
	"os"
	"path/filepath"
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
		StorageBackend:  "local",
		S3Region:        "us-east-1",
		MaxUploadMB:     50,
		FetchTimeout:    60 * time.Second,
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

func TestValidateSMBBackendRules(t *testing.T) {
	smbConfig := func() Config {
		c := validConfig()
		c.StorageBackend = "smb"
		c.SMBServer = "192.168.1.10"
		c.SMBShare = "attachment"
		c.SMBUsername = "svc"
		c.SMBPassword = "secret"
		c.SMBStagingDir = "staging"
		return c
	}

	c := smbConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("valid smb config rejected: %v", err)
	}

	for _, drop := range []string{"SMBServer", "SMBShare", "SMBUsername", "SMBPassword"} {
		c := smbConfig()
		switch drop {
		case "SMBServer":
			c.SMBServer = ""
		case "SMBShare":
			c.SMBShare = ""
		case "SMBUsername":
			c.SMBUsername = ""
		case "SMBPassword":
			c.SMBPassword = ""
		}
		if err := c.Validate(); err == nil {
			t.Errorf("expected error when %s is empty", drop)
		}
	}

	for _, bad := range []string{"/abs", `back\slash`, "up/../dir", "with..dots"} {
		c := smbConfig()
		c.SMBStagingDir = bad
		if err := c.Validate(); err == nil {
			t.Errorf("expected error for staging dir %q", bad)
		}
		c = smbConfig()
		c.SMBBaseDir = bad
		if err := c.Validate(); err == nil {
			t.Errorf("expected error for base dir %q", bad)
		}
	}

	c = smbConfig()
	c.SMBStagingDir = "sales/staged"
	if err := c.Validate(); err != nil {
		t.Fatalf("nested staging dir rejected: %v", err)
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

func TestLoadReadsDotEnvAndEnvWins(t *testing.T) {
	dir := t.TempDir()
	content := "APP_ENV=dev\nADDR=:8080\nAPI_KEYS=from-file\nMAX_UPLOAD_MB=7\n" +
		"STORAGE_BACKEND=smb\nSMB_SERVER=10.0.0.1\nSMB_SHARE=nas\n" +
		"SMB_USERNAME=u\nSMB_PASSWORD=p\nDB_PATH=./data/attachments.db\n" +
		"STORAGE_DIR=./data/storage\nSTAGING_DIR=./data/staging\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	keys := []string{"APP_ENV", "ADDR", "API_KEYS", "MAX_UPLOAD_MB", "STORAGE_BACKEND",
		"SMB_SERVER", "SMB_SHARE", "SMB_USERNAME", "SMB_PASSWORD", "DB_PATH",
		"STORAGE_DIR", "STAGING_DIR"}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(wd)
		for _, k := range keys {
			_ = os.Unsetenv(k)
		}
	})
	for _, k := range keys {
		_ = os.Unsetenv(k)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	c, err := Load()
	if err != nil {
		t.Fatalf("load .env: %v", err)
	}
	if c.StorageBackend != "smb" || c.MaxUploadMB != 7 || len(c.APIKeys) != 1 || c.APIKeys[0] != "from-file" {
		t.Fatalf("file values not applied: backend=%q max=%d keys=%v", c.StorageBackend, c.MaxUploadMB, c.APIKeys)
	}

	// A real environment variable overrides the file value.
	t.Setenv("STORAGE_BACKEND", "local")
	c, err = Load()
	if err != nil {
		t.Fatalf("load with env override: %v", err)
	}
	if c.StorageBackend != "local" {
		t.Fatalf("env did not override .env: %q", c.StorageBackend)
	}
}

func TestLoadFromExplicitPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host.env")
	content := "APP_ENV=dev\nAPI_KEYS=from-explicit-file\nMAX_UPLOAD_MB=9\n" +
		"STORAGE_BACKEND=local\nDB_PATH=./data/attachments.db\n" +
		"STORAGE_DIR=./data/storage\nSTAGING_DIR=./data/staging\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// Keep the explicit path out of the way of other tests' cwd .env.
	t.Setenv("CONFIG_FILE", path)
	c, err := LoadFrom("")
	if err != nil {
		t.Fatalf("load explicit path: %v", err)
	}
	if c.MaxUploadMB != 9 || len(c.APIKeys) != 1 || c.APIKeys[0] != "from-explicit-file" {
		t.Fatalf("explicit file not applied: max=%d keys=%v", c.MaxUploadMB, c.APIKeys)
	}

	if _, err := LoadFrom(filepath.Join(dir, "missing.env")); err == nil {
		t.Fatal("expected error for a missing explicit config file")
	}
}

func TestValidateDBPathMustBeLocal(t *testing.T) {
	for _, bad := range []string{`\\nas\share\attachments.db`, "//nas/share/attachments.db"} {
		c := validConfig()
		c.DBPath = bad
		if err := c.Validate(); err == nil {
			t.Errorf("expected error for remote DB_PATH %q", bad)
		}
	}
}
