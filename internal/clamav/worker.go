package clamav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"hhc/asset-api/internal/assets"
)

type Worker struct {
	repository Repository
	blobs      assets.BlobStore
	scanner    Scanner
	maxRetries int
	lease      time.Duration
	now        func() time.Time
}

type Repository interface {
	ClaimPendingScan(context.Context, time.Time, time.Duration) (assets.Asset, bool, error)
	ApplyScanResult(context.Context, assets.ScanResult, time.Time) (bool, error)
	ScheduleScanRetry(context.Context, string, int, string, time.Time, time.Time) error
}

type Scanner interface {
	Scan(context.Context, io.Reader, int64) (string, error)
}

func NewWorker(repository Repository, blobs assets.BlobStore, scanner Scanner, maxRetries int, scanTimeout time.Duration) *Worker {
	return &Worker{repository: repository, blobs: blobs, scanner: scanner, maxRetries: maxRetries, lease: scanTimeout + 30*time.Second, now: time.Now}
}

func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		processed, err := w.processNext(ctx)
		if err != nil {
			slog.Error("process ClamAV scan", "error", err)
		}
		if processed {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (w *Worker) processNext(ctx context.Context) (bool, error) {
	now := w.now().UTC()
	asset, found, err := w.repository.ClaimPendingScan(ctx, now, w.lease)
	if err != nil || !found {
		return false, err
	}
	download, err := w.blobs.Open(ctx, asset.ObjectKey, assets.ByteRange{}, asset.ETag)
	if err == nil {
		name, scanErr := w.scanner.Scan(ctx, download.Body, asset.SizeBytes)
		closeErr := download.Body.Close()
		if scanErr == nil && closeErr != nil {
			scanErr = closeErr
		}
		if errors.Is(scanErr, ErrInfected) {
			result := assets.ScanResult{EventID: scanEventID(asset), AssetID: asset.ID, Status: assets.ScanInfected, Details: name, ETag: asset.ETag, ExpectedAttempt: asset.ScanAttempts}
			return true, w.repositoryResult(ctx, result)
		}
		err = scanErr
	}
	if err == nil {
		result := assets.ScanResult{EventID: scanEventID(asset), AssetID: asset.ID, Status: assets.ScanClean, ETag: asset.ETag, ExpectedAttempt: asset.ScanAttempts}
		return true, w.repositoryResult(ctx, result)
	}
	details := safeDetails(err)
	if asset.ScanAttempts >= w.maxRetries {
		result := assets.ScanResult{EventID: scanEventID(asset), AssetID: asset.ID, Status: assets.ScanFailed, Details: details, ETag: asset.ETag, ExpectedAttempt: asset.ScanAttempts}
		return true, w.repositoryResult(ctx, result)
	}
	backoff := time.Duration(1<<min(asset.ScanAttempts-1, 6)) * 15 * time.Second
	return true, w.repository.ScheduleScanRetry(ctx, asset.ID, asset.ScanAttempts, details, now.Add(backoff), now)
}

func (w *Worker) repositoryResult(ctx context.Context, result assets.ScanResult) error {
	_, err := w.repository.ApplyScanResult(ctx, result, w.now().UTC())
	return err
}

func scanEventID(asset assets.Asset) string {
	if asset.ScanEventID != "" {
		return asset.ScanEventID
	}
	return fmt.Sprintf("clamav:%s:%s:%d", asset.ID, asset.ETag, asset.ScanAttempts)
}
func safeDetails(err error) string {
	if err == nil {
		return ""
	}
	value := err.Error()
	if len(value) > 500 {
		value = value[:500]
	}
	return value
}
