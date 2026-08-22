package derivatives

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"path"
	"time"

	"hhc/asset-api/internal/assets"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

var variants = []struct {
	name  string
	width int
}{{"small", 480}, {"medium", 960}, {"large", 1440}}

type Worker struct {
	repository Repository
	blobs      BlobStore
	now        func() time.Time
}

type ProcessState string

const (
	ProcessSatisfied ProcessState = "satisfied"
	ProcessRetry     ProcessState = "retry"
	ProcessTerminal  ProcessState = "terminal"
)

type ProcessResult struct {
	State   ProcessState
	RetryAt time.Time
	Attempt int
	Details string
}

type Repository interface {
	ClaimPendingProcessing(context.Context, time.Time, time.Duration) (assets.Asset, bool, error)
	ClaimProcessing(context.Context, string, string, time.Time, time.Duration) (assets.Asset, assets.ProcessingClaimState, error)
	CompleteProcessing(context.Context, string, string, int, []assets.Derivative, time.Time) error
	FailProcessing(context.Context, string, string, int, string, time.Time) error
	ScheduleProcessingRetry(context.Context, string, string, int, string, time.Time, time.Time) error
}

type BlobStore interface {
	Open(context.Context, string, assets.ByteRange, string) (assets.BlobDownload, error)
	Put(context.Context, string, io.Reader, int64, string) (assets.BlobProperties, error)
	Delete(context.Context, string) error
}

func NewWorker(repository Repository, blobs BlobStore) *Worker {
	return &Worker{repository: repository, blobs: blobs, now: time.Now}
}

func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := w.ProcessOne(ctx); err != nil && ctx.Err() == nil {
			slog.Error("process image derivatives", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *Worker) ProcessOne(ctx context.Context) error {
	asset, ok, err := w.repository.ClaimPendingProcessing(ctx, w.now().UTC(), 3*time.Minute)
	if err != nil || !ok {
		return err
	}
	_, err = w.processClaimed(ctx, asset, true)
	return err
}

func (w *Worker) ProcessAsset(ctx context.Context, assetID, etag string) (ProcessResult, error) {
	if assetID == "" || etag == "" {
		return ProcessResult{}, assets.ErrInvalidInput
	}
	now := w.now().UTC()
	asset, state, err := w.repository.ClaimProcessing(ctx, assetID, etag, now, 3*time.Minute)
	if err != nil {
		return ProcessResult{}, err
	}
	switch state {
	case assets.ProcessingClaimed:
		return w.processClaimed(ctx, asset, false)
	case assets.ProcessingDeferred:
		retryAt := maxTime(asset.ProcessingNextAt, asset.ProcessingClaimedUntil)
		if !retryAt.After(now) {
			retryAt = now.Add(15 * time.Second)
		}
		return ProcessResult{State: ProcessRetry, RetryAt: retryAt}, nil
	case assets.ProcessingTerminal:
		if asset.ID == assetID && asset.ETag == etag && (asset.ProcessingStatus == assets.ProcessingFailed || (asset.ProcessingStatus == assets.ProcessingPending && asset.ProcessingAttempts >= 5)) {
			return ProcessResult{State: ProcessTerminal, Attempt: asset.ProcessingAttempts, Details: asset.ProcessingError}, nil
		}
		return ProcessResult{State: ProcessSatisfied}, nil
	default:
		return ProcessResult{}, errors.New("unknown processing claim state")
	}
}

func (w *Worker) processClaimed(ctx context.Context, asset assets.Asset, persistTerminal bool) (ProcessResult, error) {
	written, err := w.process(ctx, asset)
	if err == nil {
		return ProcessResult{State: ProcessSatisfied}, nil
	}
	if errors.Is(err, assets.ErrCommitOutcomeUnknown) {
		return ProcessResult{}, err
	}
	for _, key := range written {
		err = errors.Join(err, w.blobs.Delete(ctx, key))
	}
	now := w.now().UTC()
	details := truncate(err.Error(), 500)
	var invalid terminalError
	if errors.As(err, &invalid) || asset.ProcessingAttempts >= 5 {
		if persistTerminal {
			return ProcessResult{}, errors.Join(err, w.repository.FailProcessing(ctx, asset.ID, asset.ETag, asset.ProcessingAttempts, details, now))
		}
		return ProcessResult{State: ProcessTerminal, Attempt: asset.ProcessingAttempts, Details: details}, nil
	}
	nextAttempt := now.Add(time.Minute << (asset.ProcessingAttempts - 1))
	if scheduleErr := w.repository.ScheduleProcessingRetry(ctx, asset.ID, asset.ETag, asset.ProcessingAttempts, details, nextAttempt, now); scheduleErr != nil {
		return ProcessResult{}, errors.Join(err, scheduleErr)
	}
	if persistTerminal {
		return ProcessResult{}, err
	}
	return ProcessResult{State: ProcessRetry, RetryAt: nextAttempt}, nil
}

func maxTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}

func (w *Worker) process(ctx context.Context, asset assets.Asset) ([]string, error) {
	download, err := w.blobs.Open(ctx, asset.ObjectKey, assets.ByteRange{}, asset.ETag)
	if err != nil {
		return nil, fmt.Errorf("open original: %w", err)
	}
	defer download.Body.Close()
	encoded, err := io.ReadAll(io.LimitReader(download.Body, (25<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("read image: %w", err)
	}
	if len(encoded) > 25<<20 {
		return nil, terminalError{fmt.Errorf("read image: %w", assets.ErrInvalidUpload)}
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(encoded))
	if err != nil {
		return nil, terminalError{fmt.Errorf("decode image config: %w", err)}
	}
	if err := validateImageConfig(config); err != nil {
		return nil, terminalError{err}
	}
	decoded, _, err := image.Decode(bytes.NewReader(encoded))
	if err != nil {
		return nil, terminalError{fmt.Errorf("decode image: %w", err)}
	}
	source := decoded.Bounds()
	if source.Dx() < 1 || source.Dy() < 1 {
		return nil, terminalError{fmt.Errorf("invalid image dimensions")}
	}
	now := w.now().UTC()
	values := make([]assets.Derivative, 0, len(variants))
	written := make([]string, 0, len(variants))
	for _, variant := range variants {
		width := min(variant.width, source.Dx())
		height := max(1, source.Dy()*width/source.Dx())
		target := image.NewRGBA(image.Rect(0, 0, width, height))
		draw.CatmullRom.Scale(target, target.Bounds(), decoded, source, draw.Over, nil)
		var encoded bytes.Buffer
		if err := jpeg.Encode(&encoded, target, &jpeg.Options{Quality: 88}); err != nil {
			return written, terminalError{fmt.Errorf("encode %s: %w", variant.name, err)}
		}
		objectKey := path.Join(path.Dir(asset.ObjectKey), "derivatives", fmt.Sprintf("attempt-%d", asset.ProcessingAttempts), variant.name+".jpg")
		written = append(written, objectKey)
		properties, err := w.blobs.Put(ctx, objectKey, bytes.NewReader(encoded.Bytes()), int64(encoded.Len()), "image/jpeg")
		if err != nil {
			return written, fmt.Errorf("store %s: %w", variant.name, err)
		}
		values = append(values, assets.Derivative{AssetID: asset.ID, Variant: variant.name, ObjectKey: objectKey, MIMEType: "image/jpeg", Width: width, Height: height, SizeBytes: properties.Size, ETag: properties.ETag, CreatedAt: now})
	}
	if err := w.repository.CompleteProcessing(ctx, asset.ID, asset.ETag, asset.ProcessingAttempts, values, now); err != nil {
		return written, fmt.Errorf("complete processing: %w", err)
	}
	return written, nil
}

func validateImageConfig(config image.Config) error {
	if config.Width < 1 || config.Height < 1 || config.Width > 8192 || config.Height > 8192 || int64(config.Width)*int64(config.Height) > 40_000_000 {
		return fmt.Errorf("invalid image dimensions")
	}
	return nil
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

type terminalError struct{ error }
