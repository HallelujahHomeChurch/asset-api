package recordingvalidation

import (
	"context"
	"errors"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
)

func RunPackageValidation(ctx context.Context, repository *postgres.RecordingPackageStore, objects PackageObjects, probe PackageMediaProbe) error {
	claim, err := repository.ClaimPackageValidation(ctx)
	if errors.Is(err, assets.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return RunProcessingClaim(ctx, func(ctx context.Context) error {
		return repository.HeartbeatPackageValidation(ctx, claim.Package.ID, claim.ClaimID)
	}, func(ctx context.Context) error {
		_, err := FreezeRecordingPackage(ctx, claim.Package, claim.ClaimID, objects, probe.Validate)
		return err
	}, func(ctx context.Context, ready bool, failure string) error {
		return repository.FinishPackageValidation(ctx, claim.Package.ID, claim.ClaimID, ready, failure)
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
	failure := "retry"
	if errors.Is(err, assets.ErrInvalidUpload) || errors.Is(err, assets.ErrInvalidInput) || errors.Is(err, assets.ErrRecordingPackageTooLarge) || errors.Is(err, assets.ErrRecordingPackageEstimateTooLarge) {
		failure = "invalid"
	}
	return finish(ctx, false, failure)
}
