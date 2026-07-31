package derivatives

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"strings"
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
	repo := &processingRepository{asset: processingAsset(1)}
	blobs := &processingBlobs{objects: map[string][]byte{repo.asset.ObjectKey: testImage(t)}}
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
	if _, ok := blobs.objects["assets/cms.news.cover/asset-1/derivatives/attempt-1/large.jpg"]; !ok {
		t.Fatal("missing stable large derivative")
	}
}

func TestWorkerRetriesDependencyFailureAndDeletesPartialDerivatives(t *testing.T) {
	now := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	repo := &processingRepository{asset: processingAsset(1)}
	blobs := &processingBlobs{
		objects:    map[string][]byte{repo.asset.ObjectKey: testImage(t)},
		failPutKey: "assets/cms.news.cover/asset-1/derivatives/attempt-1/medium.jpg",
	}
	worker := NewWorker(repo, blobs)
	worker.now = func() time.Time { return now }

	if err := worker.ProcessOne(context.Background()); err == nil {
		t.Fatal("expected dependency failure")
	}
	if repo.retryAt != now.Add(time.Minute) {
		t.Fatalf("retryAt=%s", repo.retryAt)
	}
	if repo.failed {
		t.Fatal("dependency failure was terminal")
	}
	for key := range blobs.objects {
		if strings.Contains(key, "/derivatives/") {
			t.Fatalf("partial derivative was not deleted: %s", key)
		}
	}
}

func TestWorkerCleanupCannotDeleteAnotherAttempt(t *testing.T) {
	repo := &processingRepository{asset: processingAsset(1)}
	publishedKey := "assets/cms.news.cover/asset-1/derivatives/attempt-2/small.jpg"
	blobs := &processingBlobs{
		objects: map[string][]byte{
			repo.asset.ObjectKey: testImage(t),
			publishedKey:         []byte("new attempt"),
		},
		failPutKey: "assets/cms.news.cover/asset-1/derivatives/attempt-1/medium.jpg",
	}
	worker := NewWorker(repo, blobs)

	if err := worker.ProcessOne(context.Background()); err == nil {
		t.Fatal("expected dependency failure")
	}
	if string(blobs.objects[publishedKey]) != "new attempt" {
		t.Fatal("cleanup deleted another attempt")
	}
}

func TestWorkerFailsInvalidImageWithoutRetry(t *testing.T) {
	repo := &processingRepository{asset: processingAsset(1)}
	blobs := &processingBlobs{objects: map[string][]byte{repo.asset.ObjectKey: []byte("not an image")}}
	worker := NewWorker(repo, blobs)

	if err := worker.ProcessOne(context.Background()); err == nil {
		t.Fatal("expected validation failure")
	}
	if !repo.failed {
		t.Fatal("validation failure was not terminal")
	}
	if !repo.retryAt.IsZero() {
		t.Fatal("validation failure was retried")
	}
}

func TestWorkerStopsRetryingAfterFifthAttempt(t *testing.T) {
	repo := &processingRepository{asset: processingAsset(5)}
	blobs := &processingBlobs{
		objects: map[string][]byte{repo.asset.ObjectKey: testImage(t)},
		openErr: errors.New("blob unavailable"),
	}
	worker := NewWorker(repo, blobs)

	if err := worker.ProcessOne(context.Background()); err == nil {
		t.Fatal("expected dependency failure")
	}
	if !repo.failed {
		t.Fatal("fifth failure was not terminal")
	}
	if !repo.retryAt.IsZero() {
		t.Fatal("fifth failure was retried")
	}
}

func TestWorkerRetriesCompletionFailureAndDeletesDerivatives(t *testing.T) {
	now := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	repo := &processingRepository{asset: processingAsset(2), completeErr: errors.New("database unavailable")}
	blobs := &processingBlobs{objects: map[string][]byte{repo.asset.ObjectKey: testImage(t)}}
	worker := NewWorker(repo, blobs)
	worker.now = func() time.Time { return now }

	if err := worker.ProcessOne(context.Background()); err == nil {
		t.Fatal("expected completion failure")
	}
	if repo.retryAt != now.Add(2*time.Minute) {
		t.Fatalf("retryAt=%s", repo.retryAt)
	}
	for key := range blobs.objects {
		if strings.Contains(key, "/derivatives/") {
			t.Fatalf("derivative was not deleted: %s", key)
		}
	}
}

func TestWorkerDoesNotCleanupUnknownCommitOutcome(t *testing.T) {
	repo := &processingRepository{asset: processingAsset(2), completeErr: assets.ErrCommitOutcomeUnknown}
	blobs := &processingBlobs{objects: map[string][]byte{repo.asset.ObjectKey: testImage(t)}}
	worker := NewWorker(repo, blobs)

	if err := worker.ProcessOne(context.Background()); !errors.Is(err, assets.ErrCommitOutcomeUnknown) {
		t.Fatalf("err=%v", err)
	}
	if repo.failed || !repo.retryAt.IsZero() {
		t.Fatal("unknown commit outcome changed processing state")
	}
	for _, variant := range variants {
		key := "assets/cms.news.cover/asset-1/derivatives/attempt-2/" + variant.name + ".jpg"
		if _, ok := blobs.objects[key]; !ok {
			t.Fatalf("unknown commit outcome deleted %s", key)
		}
	}
}

type processingRepository struct {
	asset       assets.Asset
	derivatives []assets.Derivative
	completeErr error
	retryAt     time.Time
	failed      bool
}

func (r *processingRepository) ClaimPendingProcessing(context.Context, time.Time, time.Duration) (assets.Asset, bool, error) {
	return r.asset, true, nil
}
func (r *processingRepository) CompleteProcessing(_ context.Context, _, _ string, _ int, values []assets.Derivative, _ time.Time) error {
	if r.completeErr != nil {
		return r.completeErr
	}
	r.derivatives = values
	return nil
}
func (r *processingRepository) FailProcessing(context.Context, string, string, int, string, time.Time) error {
	r.failed = true
	return nil
}
func (r *processingRepository) ScheduleProcessingRetry(_ context.Context, _, _ string, _ int, _ string, nextAttempt, _ time.Time) error {
	r.retryAt = nextAttempt
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
func (*processingRepository) ScheduleScanRetry(context.Context, string, int, string, time.Time, time.Time) error {
	return nil
}
func (*processingRepository) GetDerivative(context.Context, string, string) (assets.Derivative, error) {
	return assets.Derivative{}, nil
}

type processingBlobs struct {
	objects    map[string][]byte
	openErr    error
	failPutKey string
}

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
	if b.openErr != nil {
		return assets.BlobDownload{}, b.openErr
	}
	value := b.objects[key]
	return assets.BlobDownload{Body: io.NopCloser(bytes.NewReader(value)), Size: int64(len(value)), TotalSize: int64(len(value))}, nil
}
func (b *processingBlobs) Delete(_ context.Context, key string) error {
	delete(b.objects, key)
	return nil
}
func (b *processingBlobs) Put(_ context.Context, key string, reader io.Reader, size int64, _ string) (assets.BlobProperties, error) {
	value, _ := io.ReadAll(reader)
	if key == b.failPutKey {
		b.objects[key] = value
		return assets.BlobProperties{}, errors.New("blob unavailable")
	}
	b.objects[key] = value
	return assets.BlobProperties{Size: size, ETag: key}, nil
}

func processingAsset(attempts int) assets.Asset {
	return assets.Asset{
		ID:                 "asset-1",
		ObjectKey:          "assets/cms.news.cover/asset-1/original",
		ETag:               "original-etag",
		ProcessingStatus:   assets.ProcessingPending,
		ProcessingAttempts: attempts,
	}
}

func testImage(t *testing.T) []byte {
	t.Helper()
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
	return original.Bytes()
}
