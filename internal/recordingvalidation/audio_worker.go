package recordingvalidation

import (
	"context"
	"errors"
	"fmt"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
	"time"
)

type AudioObjects interface {
	PackageObjects
	PutPackageAudio(context.Context, string, []byte) error
}

func RunPackageAudio(ctx context.Context, repo *postgres.RecordingPackageStore, objects AudioObjects, probe PackageMediaProbe) (bool, error) {
	c, err := repo.ClaimPackageAudio(ctx)
	if errors.Is(err, assets.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, RunProcessingClaim(ctx, func(ctx context.Context) error { return repo.HeartbeatPackageAudio(ctx, c.Package.ID, c.ClaimID) }, func(ctx context.Context) error {
		return probe.GenerateAudio(ctx, c.Package, c.ClaimID, objects.PutPackageAudio)
	}, func(ctx context.Context, ready bool, _ string) error {
		return repo.FinishPackageAudio(ctx, c.Package.ID, c.ClaimID, ready, func(ctx context.Context) error {
			return objects.PutPackageAudio(ctx, c.Package.FinalPrefix+"audio/"+assets.RecordingAudioVersion+"/current.json", []byte(fmt.Sprintf(`{"attempt":"%s"}`, c.ClaimID)))
		})
	}, 30*time.Second)
}
