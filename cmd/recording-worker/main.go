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
	"hhc/asset-api/internal/recordingqueue"
	"hhc/asset-api/internal/recordingvalidation"
	azurestorage "hhc/asset-api/internal/storage/azure"
	"hhc/asset-api/internal/storage/r2"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const liveWorkerWindow = 5 * time.Hour

func recordingInvocationContext(parent context.Context, mode string) (context.Context, context.CancelFunc) {
	if mode == "maintenance" {
		return context.WithTimeout(parent, 5*time.Minute)
	}
	// Preserve each 330m media deadline and drain before the existing 360m Job.
	return context.WithTimeout(parent, 355*time.Minute)
}

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

func run(ctx context.Context) (result error) {
	started := time.Now()
	defer func() {
		slog.Info("recording_worker_exit", "mode", os.Getenv("ASSET_RECORDING_WORKER_MODE"), "elapsed_ms", time.Since(started).Milliseconds(), "failed", result != nil)
	}()
	hlsEnabled, err := config.RecordingHLSFlag()
	if err != nil {
		return err
	}
	if !hlsEnabled {
		return nil
	}
	mode, err := config.RecordingWorkerMode()
	if err != nil {
		return err
	}
	ctx, cancel := recordingInvocationContext(ctx, mode)
	defer cancel()
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

	events, err := config.RecordingEvents()
	if err != nil {
		return err
	}
	process := func(ctx context.Context, lane string) error {
		return processRecording(ctx, lane, db, objects, sourceAccount, sourceContainer)
	}
	if mode == "legacy" || mode == "maintenance" {
		if events.Enabled && mode == "legacy" {
			return errors.New("legacy worker cannot run with recording events enabled")
		}
		return process(ctx, mode)
	}
	if !events.Enabled {
		return errors.New("event worker requires recording events enabled")
	}
	queue, err := recordingqueue.NewAzureQueue(events.Queues[mode], events.Poison[mode])
	if err != nil {
		return err
	}
	return recordingqueue.NewJob(postgres.NewRecordingEventStore(db, sourceAccount != ""), queue, mode, func(ctx context.Context, lane string) error {
		if lane != mode {
			return errors.New("recording queue lane mismatch")
		}
		return process(ctx, lane)
	}).Run(ctx)
}

func processRecording(ctx context.Context, mode string, db *sql.DB, objects *r2.Store, sourceAccount, sourceContainer string) error {
	packages := postgres.NewRecordingPackageStore(db)
	probe := recordingvalidation.PackageMediaProbe{Objects: objects, FFmpeg: "/usr/bin/ffmpeg", FFprobe: "/usr/bin/ffprobe"}
	var cleanupErr error
	liveStore := postgres.NewRecordingCaptureStore(db)
	if mode == "legacy" || mode == "live" {
		liveClaims := 0
		auxiliaryClaims := 0
		liveSession := false
		acquired, err := liveStore.WithLiveWorker(ctx, func(ctx context.Context, check func(context.Context) error) (result error) {
			ctx, cancel := context.WithTimeout(ctx, liveWorkerWindow+30*time.Minute)
			defer cancel()
			started := time.Now()
			var busy time.Duration
			polls := 0
			slog.Info("recording_live_worker_start", "max_seconds", int(liveWorkerWindow.Seconds()))
			defer func() {
				slog.Info("recording_live_worker_exit", "elapsed_ms", time.Since(started).Milliseconds(),
					"processing_ms", busy.Milliseconds(), "non_processing_ms", (time.Since(started) - busy).Milliseconds(), "claims", liveClaims, "auxiliary_claims", auxiliaryClaims, "polls", polls, "failed", result != nil)
			}()
			// Stay across cron boundaries without changing claim/slot concurrency.
			// Use gaps between fragments for optional media so a background
			// validation cannot monopolize the other global slot for a whole live.
			return drainLiveValidation(ctx, started.Add(liveWorkerWindow), 5*time.Second,
				func(ctx context.Context) (bool, error) {
					if err := check(ctx); err != nil {
						return false, err
					}
					polls++
					before := time.Now()
					done, err := recordingvalidation.RunLiveValidation(ctx, liveStore, objects, probe)
					busy += time.Since(before)
					if done {
						liveClaims++
					}
					return done, err
				}, func(ctx context.Context) (bool, error) {
					active, err := liveStore.LiveInputPending(ctx)
					liveSession = liveSession || active
					return active, err
				}, func(ctx context.Context) bool {
					if err := check(ctx); err != nil {
						return false
					}
					before := time.Now()
					defer func() { busy += time.Since(before) }()
					processed, err := recordingvalidation.RunLiveCoverProcessing(ctx, postgres.NewRecordingLiveCoverStore(db), objects, probe)
					if err != nil {
						slog.Warn("recording_live_cover_processing_failed")
					}
					if processed || err != nil {
						if processed {
							auxiliaryClaims++
						}
						return processed
					}
					processed, err = recordingvalidation.RunLivePreview(ctx, packages, objects, probe)
					if err != nil {
						slog.Warn("recording_live_preview_processing_failed")
					}
					if processed {
						auxiliaryClaims++
					}
					return processed
				})
		})
		if err != nil {
			return err
		}
		if mode == "live" || acquired && (liveClaims > 0 || liveSession) {
			return nil
		}

	}
	if mode == "legacy" || mode == "maintenance" {
		cleanupErr = errors.Join(packages.ReconcilePackages(ctx, objects.DeletePackageObjects), liveStore.ReconcileCaptures(ctx, objects.DeletePackageObjects), liveStore.ReconcileLive(ctx, objects.DeletePackageObjects))
		liveCovers := postgres.NewRecordingLiveCoverStore(db)
		cleanupErr = errors.Join(cleanupErr, liveCovers.Reconcile(ctx, objects.DeleteCoverObjects))
	}
	liveCovers := postgres.NewRecordingLiveCoverStore(db)
	covers := postgres.NewRecordingCoverStore(db)
	if mode == "legacy" {
		if _, err := recordingvalidation.RunLiveCoverProcessing(ctx, liveCovers, objects, probe); err != nil {
			slog.Warn("recording_live_cover_processing_failed")
		}
	}
	if mode == "legacy" || mode == "maintenance" {
		cleanupErr = errors.Join(cleanupErr, covers.Reconcile(ctx, objects.DeleteCoverObjects))
	}
	if sourceAccount != "" && (mode == "legacy" || mode == "processing" || mode == "maintenance") {
		sources, err := azurestorage.New(sourceAccount, sourceContainer)
		if err != nil {
			return errors.Join(cleanupErr, err)
		}
		repository := postgres.NewRecordingSourceStore(db)
		if mode != "processing" {
			cleanupErr = errors.Join(cleanupErr, repository.ReconcileSources(ctx, sources.DeleteRecordingSource, objects.DeleteSourceAttempt))
		}
		if mode != "maintenance" {
			// One long processing claim per execution keeps the 5.5-hour app
			// deadline inside the six-hour platform Job timeout.
			processed, err := recordingprocessing.RunSourceProcessing(ctx, repository, sources, objects, probe)
			if processed || err != nil {
				return errors.Join(cleanupErr, err)
			}
		}
	}
	if mode == "maintenance" {
		return errors.Join(cleanupErr, postgres.NewRecordingEventStore(db, sourceAccount != "").Sweep(ctx))
	}
	if mode == "legacy" || mode == "preview" {
		// Preview claims defer to waiting validation/source work in SQL and
		// use the same global slots. Only one long claim runs per execution.
		if mode == "legacy" {
			if _, err := recordingvalidation.RunLivePreview(ctx, packages, objects, probe); err != nil {
				slog.Warn("recording_live_preview_processing_failed")
			}
		}
		processed, coverErr := recordingvalidation.RunCoverProcessing(ctx, covers, packages, liveCovers, objects, probe)
		if processed || coverErr != nil {
			return errors.Join(cleanupErr, coverErr)
		}
		processed, previewErr := recordingvalidation.RunPackagePreview(ctx, packages, objects, probe)
		if processed || previewErr != nil {
			return errors.Join(cleanupErr, previewErr)
		}
	}
	if mode == "preview" {
		return cleanupErr
	}
	validationErr := recordingvalidation.RunPackageValidation(ctx, packages, objects, probe)
	return errors.Join(validationErr, cleanupErr)
}

// Keep a scheduled execution available while a live input is assembling its
// next complete segment; idle sites still exit immediately.
func drainLiveValidation(ctx context.Context, deadline time.Time, interval time.Duration, run func(context.Context) (bool, error), pending func(context.Context) (bool, error), optional func(context.Context) bool) error {
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		processed, err := run(ctx)
		if err != nil {
			return err
		}
		// Live auxiliary work is owned here even before input starts or after
		// Stop/seal. SQL claims retain validation priority and the shared slot cap.
		auxiliary := optional(ctx)
		if processed || auxiliary {
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
