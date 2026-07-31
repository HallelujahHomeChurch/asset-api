package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

type Candidate struct {
	AssetID string
	Keys    []string
}

type Repository interface {
	ClaimPurge(context.Context, time.Time, time.Duration) (Candidate, bool, error)
	CompletePurge(context.Context, string, time.Time) error
	RetryPurge(context.Context, string, string, time.Time, time.Time) error
	DeleteExpiredPurge(context.Context, time.Time, int) (int64, error)
}

type BlobStore interface {
	Delete(context.Context, string) error
}

type Worker struct {
	repository Repository
	blobs      BlobStore
	now        func() time.Time
}

func NewWorker(repository Repository, blobs BlobStore) *Worker {
	return &Worker{repository: repository, blobs: blobs, now: time.Now}
}

func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		processed, err := w.ProcessOne(ctx)
		if err != nil {
			slog.Error("purge asset", "error", err)
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

func (w *Worker) ProcessOne(ctx context.Context) (bool, error) {
	now := w.now().UTC()
	candidate, found, err := w.repository.ClaimPurge(ctx, now, time.Minute)
	if err != nil {
		return false, err
	}
	if !found {
		deleted, err := w.repository.DeleteExpiredPurge(ctx, now.Add(-180*24*time.Hour), 100)
		return deleted > 0, err
	}
	for _, key := range candidate.Keys {
		if key == "" {
			continue
		}
		if err := w.blobs.Delete(ctx, key); err != nil {
			details := truncate(err.Error(), 500)
			retryErr := w.repository.RetryPurge(ctx, candidate.AssetID, details, now.Add(time.Minute), now)
			return true, errors.Join(fmt.Errorf("delete %s: %w", key, err), retryErr)
		}
	}
	return true, w.repository.CompletePurge(ctx, candidate.AssetID, now)
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
