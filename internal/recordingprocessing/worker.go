package recordingprocessing

import (
	"context"
	"errors"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
	"hhc/asset-api/internal/recordingvalidation"
)

func RunSourceProcessing(ctx context.Context, repository *postgres.RecordingSourceStore, sources SourceFinalizationObjects, outputs interface {
	RecordingOutputObjects
	recordingvalidation.PackageObjects
}, probe recordingvalidation.PackageMediaProbe) (bool, error) {
	claim, err := repository.ClaimSourceProcessing(ctx)
	if errors.Is(err, assets.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var inventory assets.RecordingPackageInventory
	var processingErr error
	return true, recordingvalidation.RunProcessingClaim(ctx, func(ctx context.Context) error {
		return repository.HeartbeatSourceProcessing(ctx, claim.Source.ID, claim.ClaimID)
	}, func(ctx context.Context) error {
		inventory, processingErr = processSourceClaim(ctx, claim, func(ctx context.Context, copy assets.RecordingSourceCopy) error {
			return repository.CheckpointSourceCopy(ctx, claim.Source.ID, claim.ClaimID, copy)
		}, sources, outputs, probe)
		return processingErr
	}, func(ctx context.Context, ready bool, failure string) error {
		if ready {
			return repository.FinishSourceProcessing(ctx, claim.Source.ID, claim.ClaimID, inventory)
		}
		if errors.Is(processingErr, ErrSourceCopyFailed) {
			failure = "copy"
		}
		return repository.FailSourceProcessing(ctx, claim.Source.ID, claim.ClaimID, failure)
	}, 30*time.Second)
}

func processSourceClaim(ctx context.Context, claim postgres.RecordingSourceClaim, checkpoint func(context.Context, assets.RecordingSourceCopy) error, sources SourceFinalizationObjects, outputs interface {
	RecordingOutputObjects
	recordingvalidation.PackageObjects
}, probe recordingvalidation.PackageMediaProbe) (inventory assets.RecordingPackageInventory, result error) {
	p := claim.Source
	if p.SourceVerifiedAt == nil {
		copy, err := FinalizeSource(ctx, p, p.CopyAttemptID, sources)
		if err != nil {
			return inventory, err
		}
		if err := checkpoint(ctx, copy); err != nil {
			return inventory, err
		}
		p.SourceKey, p.SourceETag = copy.Key, copy.ETag
	}
	reader, err := NewSourceReader(ctx, sources, p.SourceKey, p.SizeBytes, p.SourceETag)
	if err != nil {
		return inventory, err
	}
	defer func() { result = errors.Join(result, reader.Close()) }()
	spool, err := NewOutputSpool(ctx, outputs, claim.ClaimID, probe.ScratchRoot)
	if err != nil {
		return inventory, err
	}
	defer func() { result = errors.Join(result, spool.Close()) }()
	plan, err := EncodeSource(ctx, reader, spool, probe.FFmpeg, probe.FFprobe)
	if err != nil {
		return inventory, err
	}
	probe.Objects = outputs
	inventory, err = BuildSourcePackage(ctx, plan, spool, probe)
	if err != nil {
		return inventory, err
	}
	size, err := assets.ValidateRecordingInventory(inventory)
	if err != nil {
		return inventory, err
	}
	pkg := assets.RecordingPackage{ID: claim.ClaimID, Inventory: inventory, SizeBytes: size}
	_, err = recordingvalidation.FreezeRecordingPackage(ctx, pkg, claim.ClaimID, outputs, probe.Validate)
	return inventory, err
}
