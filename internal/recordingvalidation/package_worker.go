package recordingvalidation

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
)

func RunPackageValidation(ctx context.Context, repository *postgres.RecordingPackageStore, objects PackageObjects, probe PackageMediaProbe) (result error) {
	claim, err := repository.ClaimPackageValidation(ctx)
	if errors.Is(err, assets.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	started := time.Now()
	logger := slog.Default().With("package_id", claim.Package.ID, "claim_id", claim.ClaimID, "attempt", claim.Package.ProcessingProgress.Attempt)
	logger.Info("recording_package_claim", "objects_total", len(claim.Package.Inventory.Objects), "bytes_total", claim.Package.SizeBytes)
	defer func() { logPackageResult(logger, "recording_package_claim_exit", started, result) }()
	progress := NewProcessingProgressTracker(*claim.Package.ProcessingProgress)
	timing := NewAttemptTiming("package_validation")
	probe.OnStage = timing.Stage
	var lastLog time.Time
	var lastPhase, lastRendition string
	probe.OnProgress = func(value assets.RecordingProcessingProgress) {
		progress.Report(value)
		timing.Phase(value.Phase)
		if time.Since(lastLog) < 30*time.Second && value.Phase == lastPhase && value.Rendition == lastRendition {
			return
		}
		lastLog, lastPhase, lastRendition = time.Now(), value.Phase, value.Rendition
		logPackageProgress(logger, started, value)
	}
	flush := func(ctx context.Context) error {
		defer timing.Flush(ctx, func(ctx context.Context, value assets.RecordingAttemptDiagnostics) error {
			return repository.UpdatePackageAttemptDiagnostics(ctx, claim.Package.ID, claim.ClaimID, value)
		})
		return progress.Flush(ctx, time.Now().UTC(), func(ctx context.Context, value assets.RecordingProcessingProgress) error {
			return packageStageError("progress_flush", repository.UpdatePackageProcessingProgress(ctx, claim.Package.ID, claim.ClaimID, value))
		})
	}
	return RunProcessingClaim(ctx, func(ctx context.Context) error {
		return heartbeatPackageProgress(ctx, func(ctx context.Context) error {
			return repository.HeartbeatPackageValidation(ctx, claim.Package.ID, claim.ClaimID)
		}, flush, logger)
	}, func(ctx context.Context) error {
		logger.Info("recording_package_validation_start")
		_, err := FreezeRecordingPackage(ctx, claim.Package, claim.ClaimID, objects, probe)
		timing.Finish(err)
		logPackageResult(logger, "recording_package_validation_result", started, err)
		// Worker durations overlap; totals attribute work, not wall-clock latency.
		for _, stage := range []string{"freeze_hash", "probe", "decode"} {
			logger.Info("recording_package_stage_totals", "stage", stage, "worker_elapsed_ms", timing.Snapshot().Stages[stage].ElapsedMS)
		}
		return err
	}, func(ctx context.Context, ready bool, failure string) error {
		if err := flush(ctx); err != nil {
			return err
		}
		err := repository.FinishPackageValidation(ctx, claim.Package.ID, claim.ClaimID, ready, failure)
		logger.Info("recording_package_finish", "elapsed_ms", time.Since(started).Milliseconds(), "ready", ready, "failure", failure, "committed", err == nil)
		return packageStageError("finish", err)
	}, 30*time.Second)
}

// RunProcessingClaim shares the processing deadline and lease cancellation
// between direct HLS validation and browser source conversion.
func RunProcessingClaim(ctx context.Context, heartbeat, validate func(context.Context) error, finish func(context.Context, bool, string) error, interval time.Duration) error {
	jobCtx, cancel := context.WithTimeout(ctx, 330*time.Minute)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-jobCtx.Done():
				done <- nil
				return
			case <-ticker.C:
				beatCtx, beatCancel := context.WithTimeout(jobCtx, 10*time.Second)
				err := heartbeat(beatCtx)
				beatCancel()
				if err != nil {
					cancel()
					done <- err
					return
				}
			}
		}
	}()
	err := validate(jobCtx)
	cancel()
	if beatErr := <-done; beatErr != nil {
		return beatErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return finish(ctx, true, "")
	}
	return finish(ctx, false, processingFailure(err))
}

// Display progress may lag after a timeout only while the fenced lease is renewed.
func heartbeatPackageProgress(ctx context.Context, heartbeat, flush func(context.Context) error, logger *slog.Logger) error {
	if err := heartbeat(ctx); err != nil {
		return packageStageError("heartbeat", err)
	}
	started := time.Now()
	err := flush(ctx)
	if errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) && !errors.Is(err, assets.ErrConflict) && processingFailure(err) != "invalid" {
		logPackageResult(logger, "recording_package_progress_deferred", started, packageStageError("progress_flush", err))
		return nil
	}
	return err
}
