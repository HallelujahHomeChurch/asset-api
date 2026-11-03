package recordingvalidation

import (
	"context"
	"errors"
	"fmt"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
	"log/slog"
	"time"
)

type AudioObjects interface {
	PackageObjects
	PutPackageAudio(context.Context, string, []byte) error
}

func RunPackageAudio(ctx context.Context, repo *postgres.RecordingPackageStore, objects AudioObjects, probe PackageMediaProbe) (processed bool, result error) {
	c, err := repo.ClaimPackageAudio(ctx)
	if errors.Is(err, assets.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	started := time.Now()
	logger := slog.Default().With("claim_id", c.ClaimID, "recording_id", c.Package.RecordingID)
	logger.Info("recording_audio_claim")
	var finished, committedReady bool
	defer func() {
		logger.Info("recording_audio_claim_exit", "elapsed_ms", time.Since(started).Milliseconds(),
			"finish_committed", finished, "ready", committedReady, "success", result == nil && committedReady)
	}()
	return true, RunProcessingClaim(ctx, func(ctx context.Context) error { return repo.HeartbeatPackageAudio(ctx, c.Package.ID, c.ClaimID) }, func(ctx context.Context) error {
		return probe.GenerateAudio(ctx, c.Package, c.ClaimID, objects.PutPackageAudio)
	}, func(ctx context.Context, ready bool, _ string) error {
		err := repo.FinishPackageAudio(ctx, c.Package.ID, c.ClaimID, ready, func(ctx context.Context) error {
			return objects.PutPackageAudio(ctx, c.Package.FinalPrefix+"audio/"+assets.RecordingAudioVersion+"/current.json", []byte(fmt.Sprintf(`{"attempt":"%s"}`, c.ClaimID)))
		})
		finished = err == nil
		committedReady = finished && ready
		return err
	}, 30*time.Second)
}
