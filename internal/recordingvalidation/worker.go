package recordingvalidation

import (
	"context"
	"errors"
	"io"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
)

type Repository interface {
	ClaimValidation(context.Context, time.Time, time.Duration) (postgres.RecordingValidationClaim, error)
	MarkRecordingReady(context.Context, string, string, float64) error
	FinishRecordingValidation(context.Context, string, string, bool, string, time.Time) error
}

type Worker struct {
	repository Repository
	open       func(context.Context, string) (io.ReadCloser, error)
	probe      func(context.Context, string) ([]byte, error)
	now        func() time.Time
}

func New(repository Repository, open func(context.Context, string) (io.ReadCloser, error), probe func(context.Context, string) ([]byte, error), now func() time.Time) *Worker {
	return &Worker{repository: repository, open: open, probe: probe, now: now}
}

func (w *Worker) RunOnce(ctx context.Context) error {
	claim, err := w.repository.ClaimValidation(ctx, w.now(), 20*time.Minute)
	if errors.Is(err, assets.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	jobCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	session := claim.Session
	probe, err := w.probe(jobCtx, session.ObjectKey)
	if err == nil {
		var body io.ReadCloser
		body, err = w.open(jobCtx, session.ObjectKey)
		if err == nil {
			var duration float64
			duration, err = assets.ValidateRecordingStream(body, session.SizeBytes, session.ChecksumSHA256, probe)
			if closeErr := body.Close(); err == nil {
				err = closeErr
			}
			if err == nil {
				return w.repository.MarkRecordingReady(ctx, session.ID, claim.ClaimID, duration)
			}
		}
	}
	// Do not persist provider errors: they can contain signed R2 URLs.
	permanent := claim.Attempts >= 3 || errors.Is(err, assets.ErrInvalidUpload) || errors.Is(err, assets.ErrInvalidInput)
	return w.repository.FinishRecordingValidation(ctx, session.ID, claim.ClaimID, permanent, "validation failed", w.now())
}
