// Package db opens the SQLite database (WAL mode) and provides Bun-based
// persistence for attachments.
//
// Why SQLite WAL over Postgres: storage is on the local filesystem (or a
// single S3 backend), so the service is single-instance; WAL gives concurrent
// reads with a serialized writer, which is enough at ingestion volume. The
// Bun dialect makes a later Postgres migration a configuration change.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/extra/bundebug"

	// Pure-Go SQLite driver (registers as "sqlite"); keeps CGO out of the build.
	_ "modernc.org/sqlite"

	"github.com/parakram/sap-attachment-api/internal/config"
)

// Open connects, applies pragmas and runs migrations. The caller owns Close.
func Open(cfg *config.Config) (*bun.DB, error) {
	// busy_timeout/synchronous/_txlock are per-connection: carried in the DSN so
	// every pooled connection gets them. journal_mode(WAL) is persistent.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_txlock=immediate", cfg.DBPath)

	// SQLite does not create missing parent directories.
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o750); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// One writer at a time is SQLite's model; bounded pool keeps reads concurrent.
	sqldb.SetMaxOpenConns(10)
	sqldb.SetMaxIdleConns(10)

	d := bun.NewDB(sqldb, sqlitedialect.New())
	if cfg.Env == "dev" {
		d = d.WithQueryHook(bundebug.NewQueryHook(bundebug.WithVerbose(true)))
	}
	if err := d.PingContext(context.Background()); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := runMigrations(d); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return d, nil
}
