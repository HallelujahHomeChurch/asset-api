package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"hhc/asset-api/internal/config"
	"hhc/asset-api/internal/logging"
	"hhc/asset-api/internal/postgres"
	"hhc/asset-api/internal/recordingprocessing"
	"hhc/asset-api/internal/recordingvalidation"
	azurestorage "hhc/asset-api/internal/storage/azure"
	"hhc/asset-api/internal/storage/r2"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	if err := logging.Init(); err != nil {
		slog.Error("invalid logging configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("recording validation worker stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	hlsEnabled, err := config.RecordingHLSFlag()
	if err != nil {
		return err
	}
	if !hlsEnabled {
		return nil
	}
	sourceAccount, sourceContainer, err := config.RecordingSourceStorage(hlsEnabled)
	if err != nil {
		return err
	}
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	objects, err := r2.New(os.Getenv("ASSET_R2_ACCOUNT_ID"), os.Getenv("ASSET_R2_BUCKET"), os.Getenv("ASSET_R2_ACCESS_KEY_ID"), os.Getenv("ASSET_R2_SECRET_ACCESS_KEY"))
	if err != nil {
		return fmt.Errorf("recording storage configuration: %w", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(3)
	// Observability must not prevent cleanup or processing on a transient query
	// failure. No actor, filename, URL, credential or object key enters this log.
	if health, err := postgres.RecordingHealth(ctx, db); err != nil {
		slog.Warn("recording_health_unavailable")
	} else {
		slog.Info("recording_health", "package_cleanup_overdue", health.PackageCleanupOverdue,
			"source_cleanup_overdue", health.SourceCleanupOverdue, "waiting", health.Waiting, "active_slots", health.ActiveSlots)
	}
	if hlsEnabled {
		packages := postgres.NewRecordingPackageStore(db)
		probe := recordingvalidation.PackageMediaProbe{Objects: objects, FFmpeg: "/usr/bin/ffmpeg", FFprobe: "/usr/bin/ffprobe"}
		var cleanupErr error
		// A bounded batch loop drains live backlog within the existing Job and
		// shared two-slot budget. Scheduled executions continue normal work.
		liveStore := postgres.NewRecordingCaptureStore(db)
		if err := drainLiveValidation(ctx, time.Now().Add(50*time.Second), 5*time.Second,
			func(ctx context.Context) (bool, error) {
				return recordingvalidation.RunLiveValidation(ctx, liveStore, objects, probe)
			},
			liveStore.LiveInputPending); err != nil {
			return err
		}

		cleanupErr = errors.Join(packages.ReconcilePackages(ctx, objects.DeletePackageObjects), liveStore.ReconcileCaptures(ctx, objects.DeletePackageObjects), liveStore.ReconcileLive(ctx, objects.DeletePackageObjects))
		liveCovers := postgres.NewRecordingLiveCoverStore(db)
		cleanupErr = errors.Join(cleanupErr, liveCovers.Reconcile(ctx, objects.DeleteCoverObjects))
		if _, err := recordingvalidation.RunLiveCoverProcessing(ctx, liveCovers, objects, probe); err != nil {
			slog.Warn("recording_live_cover_processing_failed")
		}
		covers := postgres.NewRecordingCoverStore(db)
		cleanupErr = errors.Join(cleanupErr, covers.Reconcile(ctx, objects.DeleteCoverObjects))
		if sourceAccount != "" {
			sources, err := azurestorage.New(sourceAccount, sourceContainer)
			if err != nil {
				return errors.Join(cleanupErr, err)
			}
			repository := postgres.NewRecordingSourceStore(db)
			cleanupErr = errors.Join(cleanupErr, repository.ReconcileSources(ctx, sources.DeleteRecordingSource, objects.DeleteSourceAttempt))
			// One long processing claim per execution keeps the 5.5-hour app
			// deadline inside the six-hour platform Job timeout.
			processed, err := recordingprocessing.RunSourceProcessing(ctx, repository, sources, objects, probe)
			if processed || err != nil {
				return errors.Join(cleanupErr, err)
			}
		}
		// Preview claims defer to waiting validation/source work in SQL and
		// use the same global slots. Only one long claim runs per execution.
		if _, err := recordingvalidation.RunLivePreview(ctx, packages, objects, probe); err != nil {
			slog.Warn("recording_live_preview_processing_failed")
		}
		processed, coverErr := recordingvalidation.RunCoverProcessing(ctx, covers, packages, liveCovers, objects, probe)
		if processed || coverErr != nil {
			return errors.Join(cleanupErr, coverErr)
		}
		processed, previewErr := recordingvalidation.RunPackagePreview(ctx, packages, objects, probe)
		if processed || previewErr != nil {
			return errors.Join(cleanupErr, previewErr)
		}
		validationErr := recordingvalidation.RunPackageValidation(ctx, packages, objects, probe)
		return errors.Join(validationErr, cleanupErr)
	}
	return nil
}

// Keep a scheduled execution available while a live input is assembling its
// next complete segment; idle sites still exit immediately.
func drainLiveValidation(ctx context.Context, deadline time.Time, interval time.Duration, run func(context.Context) (bool, error), pending func(context.Context) (bool, error)) error {
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		processed, err := run(ctx)
		if err != nil {
			return err
		}
		if processed {
			continue
		}
		active, err := pending(ctx)
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
		timer := time.NewTimer(min(interval, time.Until(deadline)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}
