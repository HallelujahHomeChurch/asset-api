package scanqueue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/clamav"
)

func TestScanJobTerminalAndRetryFlows(t *testing.T) {
	tests := []struct {
		name                           string
		message                        Message
		asset                          assets.Asset
		scanErr                        error
		scanName                       string
		wantStatus                     assets.ScanStatus
		wantAck, wantRetry, wantPoison bool
	}{
		{name: "clean", message: scanMessage(1), asset: queuedAsset(), wantStatus: assets.ScanClean, wantAck: true},
		{name: "infected", message: scanMessage(1), asset: queuedAsset(), scanErr: clamav.ErrInfected, scanName: "Eicar-Signature", wantStatus: assets.ScanInfected, wantAck: true},
		{name: "transient", message: scanMessage(1), asset: queuedAsset(), scanErr: errors.New("scanner down"), wantRetry: true},
		{name: "fifth delivery", message: scanMessage(5), asset: attemptedAsset(5), wantStatus: assets.ScanFailed, wantAck: true, wantPoison: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := &jobRepository{asset: test.asset}
			queue := &jobQueue{message: test.message}
			job := NewScanJob(repo, jobBlobs{body: []byte("clean")}, &jobScanner{name: test.scanName, err: test.scanErr}, queue, "sig-1", 100, 5, time.Minute)
			job.now = func() time.Time { return time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC) }
			if processed, err := job.RunOnce(context.Background()); err != nil || !processed {
				t.Fatalf("RunOnce = %v %v", processed, err)
			}
			if repo.result.Status != test.wantStatus {
				t.Fatalf("status = %q, want %q", repo.result.Status, test.wantStatus)
			}
			if queue.acked != test.wantAck || queue.retried != test.wantRetry || queue.poisoned != test.wantPoison {
				t.Fatalf("queue = ack:%v retry:%v poison:%v", queue.acked, queue.retried, queue.poisoned)
			}
			if test.wantStatus != "" && repo.result.Signature != "sig-1" {
				t.Fatalf("signature = %q", repo.result.Signature)
			}
		})
	}
}

func TestScanJobAcksDuplicateOrStaleEventWithoutScanning(t *testing.T) {
	asset := queuedAsset()
	asset.ScanStatus = assets.ScanClean
	repo := &jobRepository{asset: asset, claimSet: true, claim: false}
	queue := &jobQueue{message: scanMessage(2)}
	scanner := &jobScanner{}
	job := NewScanJob(repo, jobBlobs{body: []byte("clean")}, scanner, queue, "sig-1", 100, 5, time.Minute)
	if _, err := job.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !queue.acked || scanner.called {
		t.Fatalf("ack=%v scanned=%v", queue.acked, scanner.called)
	}
}

func TestScanJobForwardsDurablePoisonAfterPreviousQueueSendFailed(t *testing.T) {
	asset := queuedAsset()
	asset.ScanStatus = assets.ScanFailed
	asset.ScanEventID = "event-1"
	asset.ScanFailure = "retry_exhausted"
	repo := &jobRepository{asset: asset, claimSet: true, claim: false}
	queue := &jobQueue{message: scanMessage(6)}
	job := NewScanJob(repo, jobBlobs{}, &jobScanner{}, queue, "sig-1", 100, 5, time.Minute)
	if _, err := job.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !queue.poisoned || !queue.acked {
		t.Fatalf("poison=%v ack=%v", queue.poisoned, queue.acked)
	}
}

func TestScanJobPoisonsInvalidPayloadAndIntegrityFailure(t *testing.T) {
	queue := &jobQueue{message: Message{ID: "m", PopReceipt: "p", Body: `{}`, DequeueCount: 1}}
	job := NewScanJob(&jobRepository{}, jobBlobs{}, &jobScanner{}, queue, "sig-1", 100, 5, time.Minute)
	if _, err := job.RunOnce(context.Background()); err != nil || !queue.poisoned {
		t.Fatalf("invalid payload: poison=%v err=%v", queue.poisoned, err)
	}

	asset := queuedAsset()
	asset.ChecksumSHA256 = "wrong"
	repo := &jobRepository{asset: asset}
	queue = &jobQueue{message: scanMessage(1)}
	job = NewScanJob(repo, jobBlobs{body: []byte("clean")}, &jobScanner{}, queue, "sig-1", 100, 5, time.Minute)
	if _, err := job.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !queue.poisoned || repo.result.Status != assets.ScanFailed || repo.result.FailureCategory != "integrity" {
		t.Fatalf("queue=%+v result=%+v", queue, repo.result)
	}
}

func queuedAsset() assets.Asset {
	payload := []byte("clean")
	sum := sha256.Sum256(payload)
	return assets.Asset{ID: "asset-1", ObjectKey: "asset-1", SizeBytes: int64(len(payload)), ChecksumSHA256: hex.EncodeToString(sum[:]), ETag: "etag-1", DetectedMIMEType: "text/plain; charset=utf-8", UploadStatus: assets.UploadCompleted, ScanStatus: assets.ScanPending, ScanAttempts: 1}
}

func attemptedAsset(attempt int) assets.Asset {
	asset := queuedAsset()
	asset.ScanAttempts = attempt
	return asset
}

func scanMessage(count int64) Message {
	return Message{ID: "message-1", PopReceipt: "receipt", DequeueCount: count, Body: `{"type":"asset.scan.requested.v1","version":1,"eventId":"event-1","assetId":"asset-1","etag":"etag-1"}`}
}

type jobRepository struct {
	asset    assets.Asset
	claimSet bool
	claim    bool
	result   assets.ScanResult
	retry    string
}

func (r *jobRepository) ClaimAssetScan(context.Context, string, string, string, time.Time, time.Duration) (assets.Asset, assets.ScanClaimState, error) {
	if r.claimSet && !r.claim {
		return r.asset, assets.ScanTerminal, nil
	}
	return r.asset, assets.ScanClaimed, nil
}
func (r *jobRepository) ApplyScanResult(_ context.Context, result assets.ScanResult, _ time.Time) (bool, error) {
	r.result = result
	return true, nil
}
func (r *jobRepository) ScheduleAssetScanRetry(_ context.Context, _ string, _ int, details, _ string, _ time.Time, _ time.Time) error {
	r.retry = details
	return nil
}
func (r *jobRepository) RecordScanPoison(context.Context, assets.ScanPoison, time.Time) (bool, error) {
	return true, nil
}
func (r *jobRepository) FailScanToPoison(_ context.Context, result assets.ScanResult, _ assets.ScanPoison, _ time.Time) (bool, error) {
	r.result = result
	return true, nil
}
func (r *jobRepository) MarkScanPoisonForwarded(context.Context, string, time.Time) error { return nil }

type jobBlobs struct{ body []byte }

func (b jobBlobs) Open(context.Context, string, assets.ByteRange, string) (assets.BlobDownload, error) {
	return assets.BlobDownload{Body: io.NopCloser(bytes.NewReader(b.body)), TotalSize: int64(len(b.body)), ContentType: "application/octet-stream", ETag: "etag-1"}, nil
}

type jobScanner struct {
	called bool
	name   string
	err    error
}

func (s *jobScanner) ScanFile(context.Context, string) (string, error) {
	s.called = true
	return s.name, s.err
}

type jobQueue struct {
	message  Message
	received bool
	acked    bool
	retried  bool
	poisoned bool
}

func (q *jobQueue) Receive(context.Context, time.Duration) (Message, bool, error) {
	if q.received {
		return Message{}, false, nil
	}
	q.received = true
	return q.message, true, nil
}
func (q *jobQueue) Ack(context.Context, Message) error                  { q.acked = true; return nil }
func (q *jobQueue) Retry(context.Context, Message, time.Duration) error { q.retried = true; return nil }
func (q *jobQueue) ForwardPoison(context.Context, Message, *Event, string, time.Time) error {
	q.poisoned = true
	return nil
}
