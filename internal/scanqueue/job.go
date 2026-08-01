package scanqueue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/clamav"
)

type AssetRepository interface {
	ClaimAssetScan(context.Context, string, string, string, time.Time, time.Duration) (assets.Asset, assets.ScanClaimState, error)
	ApplyScanResult(context.Context, assets.ScanResult, time.Time) (bool, error)
	ScheduleAssetScanRetry(context.Context, string, int, string, string, time.Time, time.Time) error
	RecordScanPoison(context.Context, assets.ScanPoison, time.Time) (bool, error)
	FailScanToPoison(context.Context, assets.ScanResult, assets.ScanPoison, time.Time) (bool, error)
	MarkScanPoisonForwarded(context.Context, string, time.Time) error
}

type AssetBlobs interface {
	Open(context.Context, string, assets.ByteRange, string) (assets.BlobDownload, error)
}

type FileScanner interface {
	ScanFile(context.Context, string) (string, error)
}

type MessageQueue interface {
	Receive(context.Context, time.Duration) (Message, bool, error)
	Ack(context.Context, Message) error
	Retry(context.Context, Message, time.Duration) error
	ForwardPoison(context.Context, Message, *Event, string, time.Time) error
}

type ScanJob struct {
	repository  AssetRepository
	blobs       AssetBlobs
	scanner     FileScanner
	queue       MessageQueue
	signature   string
	maxSize     int64
	maxAttempts int64
	lease       time.Duration
	now         func() time.Time
}

func NewScanJob(repository AssetRepository, blobs AssetBlobs, scanner FileScanner, queue MessageQueue, signature string, maxSize int64, maxAttempts int, timeout time.Duration) *ScanJob {
	return &ScanJob{repository: repository, blobs: blobs, scanner: scanner, queue: queue, signature: signature, maxSize: maxSize, maxAttempts: int64(maxAttempts), lease: timeout + 30*time.Second, now: time.Now}
}

func (j *ScanJob) RunOnce(ctx context.Context) (bool, error) {
	message, ok, err := j.queue.Receive(ctx, j.lease)
	if err != nil || !ok {
		return false, err
	}
	now := j.now().UTC()
	event, err := decodeEvent(message.Body)
	if err != nil {
		poison := poisonRecord(message, nil, "invalid_payload", safeError(err))
		return true, j.poison(ctx, message, nil, poison, now, false, assets.ScanResult{})
	}
	asset, claim, err := j.repository.ClaimAssetScan(ctx, event.EventID, event.AssetID, event.ETag, now, j.lease)
	if err != nil {
		return true, err
	}
	if claim == assets.ScanBusy {
		return true, j.queue.Retry(ctx, message, 15*time.Second)
	}
	if claim == assets.ScanTerminal {
		if asset.ScanStatus == assets.ScanFailed && asset.ScanEventID == event.EventID {
			reason := asset.ScanFailure
			if reason == "" {
				reason = "failed"
			}
			poison := poisonRecord(message, &event, reason, asset.ScanDetails)
			return true, j.poison(ctx, message, &event, poison, now, false, assets.ScanResult{})
		}
		return true, j.queue.Ack(ctx, message)
	}
	if int64(asset.ScanAttempts) >= j.maxAttempts {
		result := j.result(event, asset, assets.ScanFailed, "scan retry limit reached", "retry_exhausted")
		poison := poisonRecord(message, &event, "retry_exhausted", result.Details)
		return true, j.poison(ctx, message, &event, poison, now, true, result)
	}

	filePath, category, err := j.download(ctx, asset)
	if filePath != "" {
		defer os.Remove(filePath)
	}
	if err == nil {
		var name string
		name, err = j.scanner.ScanFile(ctx, filePath)
		if errors.Is(err, clamav.ErrInfected) {
			result := j.result(event, asset, assets.ScanInfected, name, "malware")
			if _, applyErr := j.repository.ApplyScanResult(ctx, result, now); applyErr != nil {
				return true, applyErr
			}
			return true, j.queue.Ack(ctx, message)
		}
		category = "scanner_unavailable"
	}
	if err == nil {
		result := j.result(event, asset, assets.ScanClean, "", "")
		if _, err := j.repository.ApplyScanResult(ctx, result, now); err != nil {
			return true, err
		}
		return true, j.queue.Ack(ctx, message)
	}
	details := safeError(err)
	if category == "integrity" {
		result := j.result(event, asset, assets.ScanFailed, details, category)
		poison := poisonRecord(message, &event, category, details)
		return true, j.poison(ctx, message, &event, poison, now, true, result)
	}
	delay := retryDelay(asset.ScanAttempts)
	if err := j.repository.ScheduleAssetScanRetry(ctx, asset.ID, asset.ScanAttempts, details, category, now.Add(delay), now); err != nil {
		return true, err
	}
	return true, j.queue.Retry(ctx, message, delay)
}

func (j *ScanJob) poison(ctx context.Context, message Message, event *Event, poison assets.ScanPoison, now time.Time, terminal bool, result assets.ScanResult) error {
	var shouldForward bool
	var err error
	if terminal {
		shouldForward, err = j.repository.FailScanToPoison(ctx, result, poison, now)
	} else {
		shouldForward, err = j.repository.RecordScanPoison(ctx, poison, now)
	}
	if err != nil {
		return err
	}
	if shouldForward {
		if err := j.queue.ForwardPoison(ctx, message, event, poison.Reason, now); err != nil {
			return err
		}
		if err := j.repository.MarkScanPoisonForwarded(ctx, poison.PoisonID, now); err != nil {
			return err
		}
	}
	return j.queue.Ack(ctx, message)
}

func poisonRecord(message Message, event *Event, reason, details string) assets.ScanPoison {
	digest := sha256.Sum256([]byte(message.Body))
	value := assets.ScanPoison{PoisonID: message.ID + ":" + reason, Reason: reason, Details: details, DequeueCount: message.DequeueCount, SourceMessageID: message.ID, BodySHA256: hex.EncodeToString(digest[:])}
	if event != nil {
		value.EventID, value.AssetID, value.ETag = event.EventID, event.AssetID, event.ETag
	}
	return value
}

func (j *ScanJob) result(event Event, asset assets.Asset, status assets.ScanStatus, details, category string) assets.ScanResult {
	return assets.ScanResult{EventID: event.EventID, AssetID: asset.ID, Status: status, Details: details, Signature: j.signature, FailureCategory: category, ETag: asset.ETag, ExpectedAttempt: asset.ScanAttempts}
}

func (j *ScanJob) download(ctx context.Context, asset assets.Asset) (string, string, error) {
	if asset.SizeBytes <= 0 || asset.SizeBytes > j.maxSize {
		return "", "integrity", fmt.Errorf("asset size %d exceeds scan limit", asset.SizeBytes)
	}
	download, err := j.blobs.Open(ctx, asset.ObjectKey, assets.ByteRange{}, asset.ETag)
	if err != nil {
		return "", "blob_unavailable", err
	}
	defer download.Body.Close()
	if download.ETag != "" && download.ETag != asset.ETag {
		return "", "integrity", errors.New("asset ETag changed")
	}
	if download.TotalSize > 0 && download.TotalSize != asset.SizeBytes {
		return "", "integrity", errors.New("asset size changed")
	}
	header := make([]byte, min(asset.SizeBytes, 512))
	if _, err := io.ReadFull(download.Body, header); err != nil {
		return "", "blob_unavailable", err
	}
	detected := http.DetectContentType(header)
	if strings.HasPrefix(string(header), "%PDF-") {
		detected = "application/pdf"
	}
	if asset.DetectedMIMEType != "" && detected != asset.DetectedMIMEType {
		return "", "integrity", fmt.Errorf("asset MIME type changed: expected %s, detected %s", asset.DetectedMIMEType, detected)
	}
	file, err := os.CreateTemp("", "asset-scan-*")
	if err != nil {
		return "", "temporary_storage", err
	}
	path := file.Name()
	ok := false
	defer func() {
		if !ok {
			file.Close()
			os.Remove(path)
		}
	}()
	hash := sha256.New()
	source := io.MultiReader(bytes.NewReader(header), download.Body)
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(source, asset.SizeBytes+1))
	if closeErr := file.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return "", "blob_unavailable", copyErr
	}
	if written != asset.SizeBytes {
		return "", "integrity", fmt.Errorf("asset size changed: expected %d, read %d", asset.SizeBytes, written)
	}
	if asset.ChecksumSHA256 != "" && !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), asset.ChecksumSHA256) {
		return "", "integrity", errors.New("asset checksum changed")
	}
	ok = true
	return path, "", nil
}

func decodeEvent(body string) (Event, error) {
	var event Event
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil {
		return Event{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Event{}, errors.New("scan event has trailing content")
	}
	if event.Type != "asset.scan.requested.v1" || event.Version != 1 || event.EventID == "" || event.AssetID == "" || event.ETag == "" || len(event.EventID) > 200 || len(event.AssetID) > 200 || len(event.ETag) > 200 {
		return Event{}, errors.New("invalid scan event")
	}
	return event, nil
}

func safeError(err error) string {
	value := err.Error()
	if len(value) > 500 {
		value = value[:500]
	}
	return value
}
