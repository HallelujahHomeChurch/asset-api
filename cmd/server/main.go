package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/clamav"
	"hhc/asset-api/internal/config"
	"hhc/asset-api/internal/httpapi"
	"hhc/asset-api/internal/migrations"
	"hhc/asset-api/internal/postgres"
	azurestorage "hhc/asset-api/internal/storage/azure"
	localstorage "hhc/asset-api/internal/storage/local"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	if err := run(); err != nil {
		slog.Error("asset api stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	migrationCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := migrations.Run(migrationCtx, db); err != nil {
		return err
	}

	var blobStore assets.BlobStore
	var localUpload http.HandlerFunc
	if cfg.StorageBackend == "azure" {
		store, err := azurestorage.New(cfg.AzureAccountURL, cfg.AzureContainer)
		if err != nil {
			return err
		}
		blobStore = store
	} else {
		store, err := localstorage.New(cfg.LocalDirectory, cfg.LocalUploadBaseURL, cfg.LocalSigningKey)
		if err != nil {
			return err
		}
		blobStore = store
		localUpload = store.PutHandler
	}
	repository := postgres.New(db)
	service := assets.NewService(repository, blobStore, cfg.PublicBaseURL, time.Now)
	handler := httpapi.New(service, db, cfg.AllowedCallers, localUpload)
	server := &http.Server{Addr: ":" + cfg.Port, Handler: handler.Routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 2 * time.Minute, IdleTimeout: 2 * time.Minute}

	scanClient := clamav.NewClient(cfg.ClamAVHost, cfg.ClamAVPort, cfg.ClamAVTimeout, cfg.ClamAVMaxFileSize)
	scanWorker := clamav.NewWorker(repository, blobStore, scanClient, cfg.ClamAVMaxRetries, cfg.ClamAVTimeout)
	go func() {
		if err := scanWorker.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("ClamAV worker stopped", "error", err)
			stop()
		}
	}()

	serverErrors := make(chan error, 1)
	go func() {
		slog.Info("asset api listening", "port", cfg.Port, "storage", cfg.StorageBackend)
		serverErrors <- server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shutdownCancel()
	return server.Shutdown(shutdownCtx)
}
