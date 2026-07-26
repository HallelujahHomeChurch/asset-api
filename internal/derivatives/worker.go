package derivatives

import (
	"bytes"
	"context"
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

type Repository interface {
	ClaimPendingProcessing(context.Context, time.Time, time.Duration) (assets.Asset, bool, error)
	CompleteProcessing(context.Context, string, string, []assets.Derivative, time.Time) error
	FailProcessing(context.Context, string, string, string, time.Time) error
}

type BlobStore interface {
	Open(context.Context, string, assets.ByteRange, string) (assets.BlobDownload, error)
	Put(context.Context, string, io.Reader, int64, string) (assets.BlobProperties, error)
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
	if err := w.process(ctx, asset); err != nil {
		_ = w.repository.FailProcessing(ctx, asset.ID, asset.ETag, truncate(err.Error(), 500), w.now().UTC())
		return err
	}
	return nil
}

func (w *Worker) process(ctx context.Context, asset assets.Asset) error {
	download, err := w.blobs.Open(ctx, asset.ObjectKey, assets.ByteRange{}, asset.ETag)
	if err != nil {
		return fmt.Errorf("open original: %w", err)
	}
	defer download.Body.Close()
	encoded, err := io.ReadAll(io.LimitReader(download.Body, (25<<20)+1))
	if err != nil || len(encoded) > 25<<20 {
		return fmt.Errorf("read image: %w", assets.ErrInvalidUpload)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("decode image config: %w", err)
	}
	if err := validateImageConfig(config); err != nil {
		return err
	}
	decoded, _, err := image.Decode(bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("decode image: %w", err)
	}
	source := decoded.Bounds()
	if source.Dx() < 1 || source.Dy() < 1 {
		return fmt.Errorf("invalid image dimensions")
	}
	now := w.now().UTC()
	values := make([]assets.Derivative, 0, len(variants))
	for _, variant := range variants {
		width := min(variant.width, source.Dx())
		height := max(1, source.Dy()*width/source.Dx())
		target := image.NewRGBA(image.Rect(0, 0, width, height))
		draw.CatmullRom.Scale(target, target.Bounds(), decoded, source, draw.Over, nil)
		var encoded bytes.Buffer
		if err := jpeg.Encode(&encoded, target, &jpeg.Options{Quality: 88}); err != nil {
			return fmt.Errorf("encode %s: %w", variant.name, err)
		}
		objectKey := path.Join(path.Dir(asset.ObjectKey), "derivatives", variant.name+".jpg")
		properties, err := w.blobs.Put(ctx, objectKey, bytes.NewReader(encoded.Bytes()), int64(encoded.Len()), "image/jpeg")
		if err != nil {
			return fmt.Errorf("store %s: %w", variant.name, err)
		}
		values = append(values, assets.Derivative{AssetID: asset.ID, Variant: variant.name, ObjectKey: objectKey, MIMEType: "image/jpeg", Width: width, Height: height, SizeBytes: properties.Size, ETag: properties.ETag, CreatedAt: now})
	}
	if err := w.repository.CompleteProcessing(ctx, asset.ID, asset.ETag, values, now); err != nil {
		return fmt.Errorf("complete processing: %w", err)
	}
	return nil
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
