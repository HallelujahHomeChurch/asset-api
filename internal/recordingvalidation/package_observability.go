package recordingvalidation

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"hhc/asset-api/internal/assets"
)

// Keep the original cause for retry classification, but never serialize it.
type packageValidationError struct {
	stage string
	cause error
}

func (e *packageValidationError) Error() string { return e.cause.Error() }
func (e *packageValidationError) Unwrap() error { return e.cause }
func packageStageError(stage string, err error) error {
	if err == nil {
		return nil
	}
	var existing *packageValidationError
	if errors.As(err, &existing) {
		return err
	}
	return &packageValidationError{stage: stage, cause: err}
}

func processingFailure(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, assets.ErrInvalidUpload) || errors.Is(err, assets.ErrInvalidInput) || errors.Is(err, assets.ErrRecordingPackageTooLarge) || errors.Is(err, assets.ErrRecordingPackageEstimateTooLarge) {
		return "invalid"
	}
	return "retry"
}

func logPackageResult(logger *slog.Logger, event string, started time.Time, err error) {
	stage, reason := "none", "none"
	if err != nil {
		stage, reason = "unknown", "transient"
		var diagnostic *packageValidationError
		if errors.As(err, &diagnostic) {
			stage = diagnostic.stage
		}
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			reason = "deadline"
		case errors.Is(err, context.Canceled):
			reason = "canceled"
		case processingFailure(err) == "invalid":
			reason = "invalid"
		}
	}
	logger.Info(event, "elapsed_ms", time.Since(started).Milliseconds(), "stage", stage, "reason", reason, "failed", err != nil)
}

func logPackageProgress(logger *slog.Logger, started time.Time, value assets.RecordingProcessingProgress) {
	logger.Info("recording_package_progress", "elapsed_ms", time.Since(started).Milliseconds(), "phase", value.Phase, "rendition", value.Rendition,
		"objects_verified", *value.ObjectsVerified, "segments_verified", *value.SegmentsVerified, "segments_total", *value.SegmentsTotal, "bytes_verified", *value.BytesVerified)
}
