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

func RunCoverProcessing(ctx context.Context, repo *postgres.RecordingCoverStore, packages *postgres.RecordingPackageStore, liveCovers *postgres.RecordingLiveCoverStore, objects CoverObjects, probe PackageMediaProbe) (bool, error) {
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
		if c.Kind == "auto" || c.Kind == "live-auto" {
			pkg, err := packages.Get(workCtx, c.PackageID)
			if err != nil {
				return err
			}
			timeline, err := liveCovers.Timeline(workCtx, pkg.ID)
			if err != nil {
				return err
			}
			var inherited []byte
			if c.InheritedCoverID != "" {
				inherited, err = readInheritedCover(workCtx, liveCovers, objects, c)
				if err != nil {
					return err
				}
			}
			return probe.GenerateCoversWithTimeline(workCtx, pkg, c.ClaimID, inherited, timeline, objects.PutRecordingCover)
		}
		if c.InheritedCoverID != "" {
			data, err := readInheritedCover(workCtx, liveCovers, objects, c)
			if err != nil {
				return err
			}
			c.OutputAttempt = c.ClaimID
			return objects.PutRecordingCover(workCtx, c.Key(0), data)
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
	if err == nil && finishErr == nil && c.InheritedCoverID != "" {
		finishErr = liveCovers.Release(finishCtx, c.InheritedCoverID, c.PackageID, "vod:"+c.ID)
	}
	if err == nil && finishErr == nil && c.Kind == "custom" {
		finishErr = objects.DeleteCoverObjects(finishCtx, []string{c.InputKey()})
	}
	return true, errors.Join(err, finishErr)
}

func readInheritedCover(ctx context.Context, repo *postgres.RecordingLiveCoverStore, objects CoverObjects, c assets.RecordingCover) ([]byte, error) {
	source, err := repo.Get(ctx, c.InheritedCoverID, c.PackageID, c.RecordingID)
	if err != nil {
		return nil, err
	}
	if source.State != "ready" {
		return nil, assets.ErrConflict
	}
	body, err := objects.Open(ctx, source.Key())
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(body, assets.RecordingCoverOutputMaxBytes+1))
	err = errors.Join(err, body.Close())
	if err != nil {
		return nil, err
	}
	if len(data) > assets.RecordingCoverOutputMaxBytes || fmt.Sprintf("%x", sha256.Sum256(data)) != source.OutputDigest {
		return nil, assets.ErrInvalidUpload
	}
	return data, nil
}
