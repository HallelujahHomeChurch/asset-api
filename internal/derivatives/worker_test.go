package derivatives

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestValidateImageConfigRejectsExcessiveDimensions(t *testing.T) {
	for _, config := range []image.Config{
		{Width: 8193, Height: 1},
		{Width: 8000, Height: 6000},
	} {
		if err := validateImageConfig(config); err == nil {
			t.Fatalf("accepted config %+v", config)
		}
	}
	if err := validateImageConfig(image.Config{Width: 512, Height: 512}); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestWorkerCreatesStableResponsiveVariants(t *testing.T) {
	var original bytes.Buffer
	imageValue := image.NewRGBA(image.Rect(0, 0, 1200, 600))
	for y := 0; y < 600; y++ {
		for x := 0; x < 1200; x++ {
			imageValue.Set(x, y, color.RGBA{R: 180, G: 90, B: 80, A: 255})
		}
	}
	if err := png.Encode(&original, imageValue); err != nil {
		t.Fatal(err)
	}
	repo := &processingRepository{asset: assets.Asset{ID: "asset-1", ObjectKey: "assets/cms.news.cover/asset-1/original", ProcessingStatus: assets.ProcessingPending}}
	blobs := &processingBlobs{objects: map[string][]byte{repo.asset.ObjectKey: original.Bytes()}}
	worker := NewWorker(repo, blobs)
	worker.now = func() time.Time { return time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC) }

	if err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(repo.derivatives) != 3 {
		t.Fatalf("derivatives=%d", len(repo.derivatives))
	}
	if repo.derivatives[0].Variant != "small" || repo.derivatives[0].Width != 480 || repo.derivatives[0].Height != 240 {
		t.Fatalf("small=%+v", repo.derivatives[0])
	}
	if _, ok := blobs.objects["assets/cms.news.cover/asset-1/derivatives/large.jpg"]; !ok {
		t.Fatal("missing stable large derivative")
	}
}

type processingRepository struct {
	asset       assets.Asset
	derivatives []assets.Derivative
}

func (r *processingRepository) ClaimPendingProcessing(context.Context, time.Time, time.Duration) (assets.Asset, bool, error) {
	return r.asset, true, nil
}
func (r *processingRepository) CompleteProcessing(_ context.Context, _, _ string, values []assets.Derivative, _ time.Time) error {
	r.derivatives = values
	return nil
}
func (*processingRepository) FailProcessing(context.Context, string, string, string, time.Time) error {
	return nil
}
func (*processingRepository) CreateUpload(context.Context, assets.Asset, assets.UploadSession) error {
	return nil
}
func (*processingRepository) GetAsset(context.Context, string) (assets.Asset, error) {
	return assets.Asset{}, nil
}
func (*processingRepository) GetUploadSession(context.Context, string) (assets.UploadSession, error) {
	return assets.UploadSession{}, nil
}
func (*processingRepository) CompleteUpload(context.Context, assets.Asset, assets.UploadSession) error {
	return nil
}
func (*processingRepository) FailUpload(context.Context, string, time.Time) error { return nil }
func (*processingRepository) CreateGrant(context.Context, assets.Grant) (assets.Grant, error) {
	return assets.Grant{}, nil
}
func (*processingRepository) RevokeGrant(context.Context, string, string, time.Time) error {
	return nil
}
func (*processingRepository) HasActiveGrant(context.Context, string, assets.SubjectType, string, assets.Permission, time.Time) (bool, error) {
	return false, nil
}
func (*processingRepository) ApplyScanResult(context.Context, assets.ScanResult, time.Time) (bool, error) {
	return false, nil
}
func (*processingRepository) ClaimPendingScan(context.Context, time.Time, time.Duration) (assets.Asset, bool, error) {
	return assets.Asset{}, false, nil
}
func (*processingRepository) ScheduleScanRetry(context.Context, string, string, time.Time, time.Time) error {
	return nil
}
func (*processingRepository) GetDerivative(context.Context, string, string) (assets.Derivative, error) {
	return assets.Derivative{}, nil
}

type processingBlobs struct{ objects map[string][]byte }

func (*processingBlobs) CreateUploadTarget(context.Context, string, int64, time.Time) (assets.UploadTarget, error) {
	return assets.UploadTarget{}, nil
}
func (*processingBlobs) InspectProperties(context.Context, string) (assets.BlobMetadata, error) {
	return assets.BlobMetadata{}, nil
}
func (*processingBlobs) Inspect(context.Context, string, string, int64) (assets.BlobProperties, error) {
	return assets.BlobProperties{}, nil
}
func (b *processingBlobs) Open(_ context.Context, key string, _ assets.ByteRange, _ string) (assets.BlobDownload, error) {
	value := b.objects[key]
	return assets.BlobDownload{Body: io.NopCloser(bytes.NewReader(value)), Size: int64(len(value)), TotalSize: int64(len(value))}, nil
}
func (b *processingBlobs) Delete(_ context.Context, key string) error {
	delete(b.objects, key)
	return nil
}
func (b *processingBlobs) Put(_ context.Context, key string, reader io.Reader, size int64, _ string) (assets.BlobProperties, error) {
	value, _ := io.ReadAll(reader)
	b.objects[key] = value
	return assets.BlobProperties{Size: size, ETag: key}, nil
}
