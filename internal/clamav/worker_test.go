package clamav

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestWorkerMarksCleanAsset(t *testing.T) {
	repo := &workerRepository{asset: pendingAsset()}
	worker := NewWorker(repo, workerBlobs{}, scannerStub{}, 3, time.Minute)
	processed, err := worker.processNext(context.Background())
	if err != nil || !processed {
		t.Fatalf("processNext() = %v, %v", processed, err)
	}
	if repo.result.Status != assets.ScanClean {
		t.Fatalf("status = %s", repo.result.Status)
	}
	if repo.result.ExpectedAttempt != repo.asset.ScanAttempts {
		t.Fatalf("expected attempt = %d, want %d", repo.result.ExpectedAttempt, repo.asset.ScanAttempts)
	}
}

func TestWorkerMarksInfectedAssetWithSignature(t *testing.T) {
	repo := &workerRepository{asset: pendingAsset()}
	worker := NewWorker(repo, workerBlobs{}, scannerStub{name: "Eicar-Signature", err: ErrInfected}, 3, time.Minute)
	_, err := worker.processNext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if repo.result.Status != assets.ScanInfected || repo.result.Details != "Eicar-Signature" {
		t.Fatalf("result = %#v", repo.result)
	}
}

func TestWorkerRetriesUnavailableClamAVThenFailsClosed(t *testing.T) {
	repo := &workerRepository{asset: pendingAsset()}
	repo.asset.ScanAttempts = 1
	worker := NewWorker(repo, workerBlobs{}, scannerStub{err: errors.New("clamd unavailable")}, 2, time.Minute)
	_, err := worker.processNext(context.Background())
	if err != nil || repo.retry == "" || repo.result.Status != "" {
		t.Fatalf("first attempt: retry=%q result=%#v err=%v", repo.retry, repo.result, err)
	}

	repo.retry = ""
	repo.asset.ScanAttempts = 2
	_, err = worker.processNext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if repo.result.Status != assets.ScanFailed {
		t.Fatalf("status = %s", repo.result.Status)
	}
}

func pendingAsset() assets.Asset {
	return assets.Asset{ID: "asset-1", ObjectKey: "assets/asset-1/original", SizeBytes: 5, ETag: "etag", UploadStatus: assets.UploadCompleted, ScanStatus: assets.ScanPending}
}

type scannerStub struct {
	name string
	err  error
}

func (s scannerStub) Scan(_ context.Context, reader io.Reader, _ int64) (string, error) {
	_, _ = io.Copy(io.Discard, reader)
	return s.name, s.err
}

type workerBlobs struct{}

func (workerBlobs) Open(context.Context, string, assets.ByteRange, string) (assets.BlobDownload, error) {
	return assets.BlobDownload{Body: io.NopCloser(bytes.NewReader([]byte("clean"))), Size: 5}, nil
}
func (workerBlobs) CreateUploadTarget(context.Context, string, int64, time.Time) (assets.UploadTarget, error) {
	panic("not used")
}
func (workerBlobs) InspectProperties(context.Context, string) (assets.BlobMetadata, error) {
	panic("not used")
}
func (workerBlobs) Inspect(context.Context, string, string, int64) (assets.BlobProperties, error) {
	panic("not used")
}
func (workerBlobs) Commit(context.Context, string, string) (assets.BlobProperties, error) {
	panic("not used")
}
func (workerBlobs) Delete(context.Context, string) error { panic("not used") }

type workerRepository struct {
	asset  assets.Asset
	result assets.ScanResult
	retry  string
}

func (r *workerRepository) ClaimPendingScan(context.Context, time.Time, time.Duration) (assets.Asset, bool, error) {
	return r.asset, true, nil
}
func (r *workerRepository) ApplyScanResult(_ context.Context, result assets.ScanResult, _ time.Time) (bool, error) {
	r.result = result
	return true, nil
}
func (r *workerRepository) ScheduleScanRetry(_ context.Context, _ string, _ int, details string, _, _ time.Time) error {
	r.retry = details
	return nil
}
func (*workerRepository) CreateUpload(context.Context, assets.Asset, assets.UploadSession) error {
	panic("not used")
}
func (*workerRepository) GetAsset(context.Context, string) (assets.Asset, error) { panic("not used") }
func (*workerRepository) GetUploadSession(context.Context, string) (assets.UploadSession, error) {
	panic("not used")
}
func (*workerRepository) CompleteUpload(context.Context, assets.Asset, assets.UploadSession) error {
	panic("not used")
}
func (*workerRepository) FailUpload(context.Context, string, time.Time) error { panic("not used") }
func (*workerRepository) CreateGrant(context.Context, assets.Grant) (assets.Grant, error) {
	panic("not used")
}
func (*workerRepository) RevokeGrant(context.Context, string, string, time.Time) error {
	panic("not used")
}
func (*workerRepository) HasActiveGrant(context.Context, string, assets.SubjectType, string, assets.Permission, time.Time) (bool, error) {
	panic("not used")
}
