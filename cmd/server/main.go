// Command server runs the SAP attachment ingestion service.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/parakram/sap-attachment-api/internal/config"
	"github.com/parakram/sap-attachment-api/internal/db"
	"github.com/parakram/sap-attachment-api/internal/logging"
	"github.com/parakram/sap-attachment-api/internal/server"
	"github.com/parakram/sap-attachment-api/internal/service"
	"github.com/parakram/sap-attachment-api/internal/storage"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	log, closeLogs := logging.New(cfg)
	defer closeLogs(context.Background())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	database, err := db.Open(cfg)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = database.Close() }()

	repo := db.NewRepo(database)

	backend, err := storage.NewLocal(cfg.StorageDir)
	if err != nil {
		return fmt.Errorf("init storage backend: %w", err)
	}

	// Staging dir is created up front: uploads and /readyz depend on it.
	if err := os.MkdirAll(cfg.StagingDir, 0o750); err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}

	// Startup reconciliation: clean orphan staging files, resolve rows stuck
	// in staged. Failures are logged but not fatal — the service still runs.
	sweeper := service.NewSweeper(repo, backend, cfg.StagingDir, time.Hour)
	swept, err := sweeper.Run(ctx)
	if err != nil {
		log.Error("storage reconciliation failed", "err", err)
	} else {
		log.Info("storage reconciled",
			"removed_temp_files", swept.RemovedTempFiles, "resolved_rows", swept.ResolvedRows)
	}

	ready := func(ctx context.Context) error {
		if err := database.PingContext(ctx); err != nil {
			return fmt.Errorf("database: %w", err)
		}
		if st, err := os.Stat(cfg.StagingDir); err != nil || !st.IsDir() {
			return errors.New("staging directory missing")
		}
		return nil
	}

	app := server.New(cfg, log, ready)

	errCh := make(chan error, 1)
	go func() { errCh <- app.Listen(cfg.Addr) }()

	select {
	case err := <-errCh:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := app.ShutdownWithContext(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	closeLogs(shutdownCtx)
	return nil
}
