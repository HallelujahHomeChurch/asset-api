package recordingvalidation

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
	"io"
	"time"
)

type CoverObjects interface {
	PackageObjects
	PutRecordingCover(context.Context, string, []byte) error
	DeleteCoverObjects(context.Context, []string) error
}

func RunCoverProcessing(ctx context.Context, repo *postgres.RecordingCoverStore, packages *postgres.RecordingPackageStore, objects CoverObjects, probe PackageMediaProbe) (bool, error) {
	c, err := repo.Claim(ctx)
	if errors.Is(err, assets.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// A claim lasts three minutes; all work is bounded to two. A paused/killed
	// process cannot publish after the lease expires, even if its R2 PUT finishes.
	workCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	err = func() error {
		if c.Kind == "auto" {
			pkg, err := packages.Get(workCtx, c.PackageID)
			if err != nil {
				return err
			}
			return probe.GenerateCovers(workCtx, pkg, c.ClaimID, objects.PutRecordingCover)
		}
		body, err := objects.Open(workCtx, c.InputKey())
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(body, assets.RecordingCoverInputMaxBytes+1))
		err = errors.Join(err, body.Close())
		if err != nil {
			return err
		}
		if fmt.Sprintf("%x", sha256.Sum256(data)) != c.Digest {
			return assets.ErrInvalidUpload
		}
		out, err := probe.NormalizeCover(workCtx, data, c.MIME)
		if err != nil {
			return err
		}
		c.OutputAttempt = c.ClaimID
		return objects.PutRecordingCover(workCtx, c.Key(0), out)
	}()
	cancel()
	finishCtx, finishCancel := context.WithTimeout(ctx, 15*time.Second)
	defer finishCancel()
	finishErr := repo.Finish(finishCtx, c, err == nil)
	if err == nil && finishErr == nil && c.Kind == "custom" {
		finishErr = objects.DeleteCoverObjects(finishCtx, []string{c.InputKey()})
	}
	return true, errors.Join(err, finishErr)
}
