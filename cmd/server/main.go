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
	"hhc/asset-api/internal/auditclient"
	"hhc/asset-api/internal/auditoutbox"
	"hhc/asset-api/internal/clamav"
	"hhc/asset-api/internal/config"
	"hhc/asset-api/internal/dependencyhealth"
	"hhc/asset-api/internal/derivativequeue"
	"hhc/asset-api/internal/diagnostics"
	"hhc/asset-api/internal/httpapi"
	"hhc/asset-api/internal/lifecycle"
	"hhc/asset-api/internal/logging"
	"hhc/asset-api/internal/postgres"
	"hhc/asset-api/internal/recordingqueue"
	"hhc/asset-api/internal/scanqueue"
	azurestorage "hhc/asset-api/internal/storage/azure"
	localstorage "hhc/asset-api/internal/storage/local"
	r2storage "hhc/asset-api/internal/storage/r2"
	"hhc/asset-api/internal/telemetry"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	if err := logging.Init(); err != nil {
		slog.Error("invalid logging configuration", "error", err)
		os.Exit(1)
	}
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
	shutdownTelemetry := telemetry.Configure(ctx)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if shutdownTelemetry(shutdownCtx) != nil {
			slog.Warn("trace shutdown incomplete")
		}
	}()
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(cfg.DBMaxOpenConns)
	db.SetMaxIdleConns(cfg.DBMaxIdleConns)
	db.SetConnMaxLifetime(cfg.DBConnMaxLifetime)
	defer db.Close()
	stopDependencyHealth := dependencyhealth.Start(ctx, db, os.Getenv("DAPR_HTTP_PORT"), os.Getenv("DAPR_API_TOKEN"), slog.Default())
	defer stopDependencyHealth()
	diagnosticsCtx, diagnosticsCancel := context.WithCancel(ctx)
	diagnosticsDone := make(chan struct{})
	go func() {
		defer close(diagnosticsDone)
		diagnostics.Default.Run(diagnosticsCtx, db.Stats, slog.Default())
	}()
	defer func() {
		diagnosticsCancel()
		// A blocked console sink must not indefinitely hold service shutdown.
		select {
		case <-diagnosticsDone:
		case <-time.After(time.Second):
		}
	}()
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
	auditStore := auditoutbox.New(db)
	repository.WithAudit(auditStore)
	service := assets.NewService(repository, blobStore, cfg.PublicBaseURL, time.Now)
	handler := httpapi.New(service, db, cfg.AllowedCallers, cfg.AllowDevCallerHeader, cfg.AppAPIToken, workloadAuth(cfg), localUpload).WithAudit(auditStore)
	if cfg.R2AccountID != "" {
		objects, err := r2storage.New(cfg.R2AccountID, cfg.R2Bucket, cfg.R2AccessKeyID, cfg.R2SecretAccessKey)
		if err != nil {
			return err
		}
		signer, err := assets.NewRecordingSigner([]byte(cfg.MediaPrivateKeyPEM), cfg.MediaKeyID, cfg.MediaIssuer)
		if err != nil {
			return err
		}
		handler.WithRecordingSigner(signer)
		if cfg.RecordingHLSEnabled {
			handler.WithRecordingPackages(assets.NewRecordingPackageService(postgres.NewRecordingPackageStore(db).WithBroadcastObjects(objects), objects, time.Now))
			captureStore := postgres.NewRecordingCaptureStore(db).WithBroadcastObjects(objects)
			handler.WithRecordingCaptures(assets.NewRecordingCaptureService(captureStore, objects, time.Now))
			handler.WithRecordingBroadcasts(captureStore)
			handler.WithRecordingRetention(postgres.NewRecordingPackageStore(db))
			handler.WithRecordingDeletion(postgres.NewRecordingDeletionStore(db))
			handler.WithRecordingCovers(postgres.NewRecordingCoverStore(db), objects)
			handler.WithRecordingLiveCovers(postgres.NewRecordingLiveCoverStore(db))
			if cfg.RecordingSourceAccountURL != "" {
				sources, err := azurestorage.New(cfg.RecordingSourceAccountURL, cfg.RecordingSourceContainer)
				if err != nil {
					return err
				}
				handler.WithRecordingSources(assets.NewRecordingSourceService(postgres.NewRecordingSourceStore(db), sources, time.Now))
			}
		}
	}
	server := &http.Server{Addr: ":" + cfg.Port, Handler: telemetry.Server(handler.Routes()), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 2 * time.Minute, IdleTimeout: 2 * time.Minute}

	lifecycleWorker := lifecycle.NewWorker(repository, blobStore)
	if cfg.AuditDispatchEnabled {
		client, err := auditclient.New(cfg.DaprHTTPPort, cfg.AuditAppID, cfg.AuditToken)
		if err != nil {
			return err
		}
		go auditoutbox.NewWorker(auditStore, client).Run(ctx)
	}
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
	if cfg.DerivativeQueueURL != "" {
		sender, err := derivativequeue.NewAzureSender(cfg.DerivativeQueueURL)
		if err != nil {
			return err
		}
		dispatcher := derivativequeue.NewDispatcher(repository, sender, time.Now)
		go func() {
			if err := dispatcher.Run(ctx); err != nil && ctx.Err() == nil {
				slog.Error("derivative outbox dispatcher stopped", "error", err)
				stop()
			}
		}()
	}

	events, err := config.RecordingEvents()
	if err != nil {
		return err
	}
	if events.Enabled {
		if !cfg.RecordingHLSEnabled {
			return errors.New("recording events require HLS")
		}
		sender, err := recordingqueue.NewAzureSender(events.Queues)
		if err != nil {
			return err
		}
		dispatcher := recordingqueue.NewDispatcher(postgres.NewRecordingEventStore(db, cfg.RecordingSourceAccountURL != ""), sender)
		go func() {
			if err := dispatcher.Run(ctx); err != nil && ctx.Err() == nil {
				slog.Error("recording dispatcher stopped")
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

func workloadAuth(cfg config.Config) httpapi.WorkloadAuthConfig {
	callers := map[string]httpapi.WorkloadCaller{}
	if cfg.LineWorkloadClientID != "" {
		callers[cfg.LineWorkloadClientID] = httpapi.WorkloadCaller{ObjectID: cfg.LineWorkloadObjectID, Service: "hhc-line-function-bot"}
	}
	if cfg.ExtractorWorkloadClientID != "" {
		callers[cfg.ExtractorWorkloadClientID] = httpapi.WorkloadCaller{ObjectID: cfg.ExtractorWorkloadObjectID, Service: "hhc-web-api"}
	}
	if cfg.WatermarkWorkloadClientID != "" {
		callers[cfg.WatermarkWorkloadClientID] = httpapi.WorkloadCaller{ObjectID: cfg.WatermarkWorkloadObjectID, Service: "hhc-web-bulletin-worker"}
	}
	return httpapi.WorkloadAuthConfig{TenantID: cfg.WorkloadTenantID, Issuer: cfg.WorkloadIssuer, Audience: cfg.WorkloadAudience, RequiredRole: cfg.WorkloadRequiredRole, ReaderCallerAppID: cfg.ReaderCallerAppID, Callers: callers}
}
