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
	"hhc/asset-api/internal/derivatives"
	"hhc/asset-api/internal/httpapi"
	"hhc/asset-api/internal/lifecycle"
	"hhc/asset-api/internal/postgres"
	"hhc/asset-api/internal/scanqueue"
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
	db.SetMaxOpenConns(cfg.DBMaxOpenConns)
	db.SetMaxIdleConns(cfg.DBMaxIdleConns)
	db.SetConnMaxLifetime(cfg.DBConnMaxLifetime)
	defer db.Close()
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
	workloadCallers := map[string]httpapi.WorkloadCaller{}
	if cfg.LineWorkloadClientID != "" {
		workloadCallers[cfg.LineWorkloadClientID] = httpapi.WorkloadCaller{ObjectID: cfg.LineWorkloadObjectID, Service: "hhc-line-function-bot"}
	}
	handler := httpapi.New(service, db, cfg.AllowedCallers, cfg.AllowDevCallerHeader, cfg.AppAPIToken, httpapi.WorkloadAuthConfig{
		TenantID: cfg.WorkloadTenantID, Issuer: cfg.WorkloadIssuer, Audience: cfg.WorkloadAudience,
		RequiredRole: cfg.WorkloadRequiredRole, ReaderCallerAppID: cfg.ReaderCallerAppID, Callers: workloadCallers,
	}, localUpload)
	server := &http.Server{Addr: ":" + cfg.Port, Handler: handler.Routes(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 2 * time.Minute, IdleTimeout: 2 * time.Minute}

	derivativeBlobs, ok := blobStore.(derivatives.BlobStore)
	if !ok {
		return errors.New("storage backend does not support derivative writes")
	}
	derivativeWorker := derivatives.NewWorker(repository, derivativeBlobs)
	lifecycleWorker := lifecycle.NewWorker(repository, blobStore)
	if cfg.ScanDispatchEnabled {
		sender, err := scanqueue.NewAzureSender(cfg.ScanQueueURL)
		if err != nil {
			return err
		}
		dispatcher := scanqueue.NewDispatcher(repository, sender, time.Now)
		go func() {
			if err := dispatcher.Run(ctx); err != nil && ctx.Err() == nil {
				slog.Error("scan outbox dispatcher stopped", "error", err)
				stop()
			}
		}()
	}
	if cfg.EmbeddedScanEnabled {
		scanClient := clamav.NewClient(cfg.ClamAVHost, cfg.ClamAVPort, cfg.ClamAVTimeout, cfg.ClamAVMaxFileSize)
		scanWorker := clamav.NewWorker(repository, blobStore, scanClient, cfg.ClamAVMaxRetries, cfg.ClamAVTimeout)
		go func() {
			if err := scanWorker.Run(ctx); err != nil && ctx.Err() == nil {
				slog.Error("ClamAV worker stopped", "error", err)
				stop()
			}
		}()
	}
	go func() {
		if err := derivativeWorker.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("derivative worker stopped", "error", err)
			stop()
		}
	}()
	go func() {
		if err := lifecycleWorker.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("lifecycle worker stopped", "error", err)
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
