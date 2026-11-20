// Command server runs the SAP attachment ingestion service.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/parakram/sap-attachment-api/internal/config"
	"github.com/parakram/sap-attachment-api/internal/db"
	"github.com/parakram/sap-attachment-api/internal/fetch"
	"github.com/parakram/sap-attachment-api/internal/logging"
	"github.com/parakram/sap-attachment-api/internal/sap"
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
	configPath := flag.String("config", "", "path to a dotenv config file (default ./.env; CONFIG_FILE also accepted)")
	flag.Parse()

	cfg, err := config.LoadFrom(*configPath)
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

	backend, err := storage.New(cfg)
	if err != nil {
		return fmt.Errorf("init storage backend: %w", err)
	}
	log.Info("storage configured",
		"backend", cfg.StorageBackend,
		"smb_share", cfg.SMBShare,
		"smb_base_dir", cfg.SMBBaseDir)

	repo := db.NewRepo(database)

	uploader, err := service.NewUploader(repo, backend, cfg.StagingDir, cfg.MaxUploadBytes())
	if err != nil {
		return fmt.Errorf("init uploader: %w", err)
	}

	fetcher, err := fetch.New(cfg)
	if err != nil {
		return fmt.Errorf("init remote fetcher: %w", err)
	}

	sapClient, err := sap.New(cfg)
	if err != nil {
		return fmt.Errorf("init SAP client: %w", err)
	}
	go sapClient.Start(ctx) // nil-safe; keeps the SAP session warm (25 min refresh)

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

	app := server.New(server.Deps{
		Cfg:      cfg,
		Log:      log,
		Ready:    ready,
		Repo:     repo,
		Store:    backend,
		Uploader: uploader,
		Fetcher:  fetcher,
		SAP:      sapClient,
	})

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
	if err := backend.Close(); err != nil {
		log.Warn("storage backend close failed", "err", err)
	}

	closeLogs(shutdownCtx)
	return nil
}
