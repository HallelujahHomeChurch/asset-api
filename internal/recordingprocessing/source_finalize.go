package recordingprocessing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"time"

	"hhc/asset-api/internal/assets"
)

var ErrSourceCopyFailed = errors.New("recording_source_copy_failed")

type SourceFinalizationObjects interface {
	SourceObjects
	CopyRecordingSource(context.Context, assets.RecordingSource, string) (assets.RecordingSourceCopy, error)
}

// FinalizeSource waits for the existing provider copy and hashes bounded SDK
// windows without materializing the source on disk. It neither encodes nor marks
// anything ready; the caller must fence its verified-source checkpoint in DB.
func FinalizeSource(ctx context.Context, p assets.RecordingSource, attempt string, objects SourceFinalizationObjects) (assets.RecordingSourceCopy, error) {
	if assets.ValidateRecordingSourceSize(p.SizeBytes) != nil || len(p.ChecksumSHA256) != 64 {
		return assets.RecordingSourceCopy{}, assets.ErrInvalidInput
	}
	if _, err := hex.DecodeString(p.ChecksumSHA256); err != nil {
		return assets.RecordingSourceCopy{}, assets.ErrInvalidInput
	}
	var copy assets.RecordingSourceCopy
	for {
		if err := ctx.Err(); err != nil {
			return copy, err
		}
		var err error
		copy, err = objects.CopyRecordingSource(ctx, p, attempt)
		if err != nil {
			return copy, err
		}
		if copy.State == "success" {
			break
		}
		if copy.State != "pending" {
			return copy, ErrSourceCopyFailed
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return copy, ctx.Err()
		case <-timer.C:
		}
	}
	if !immutableSourceKey.MatchString(copy.Key) || copy.Key != "recording-sources/"+p.ID+"/final/"+attempt+"/source" || copy.SizeBytes != p.SizeBytes || copy.ETag == "" {
		return copy, assets.ErrInvalidUpload
	}
	reader := &sourceSeeker{ctx: ctx, objects: objects, key: copy.Key, size: p.SizeBytes, etag: copy.ETag}
	hash := sha256.New()
	n, err := io.CopyBuffer(hash, reader, make([]byte, 64<<10))
	if err = errors.Join(err, reader.Close()); err != nil {
		return copy, err
	}
	if n != p.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != p.ChecksumSHA256 {
		return copy, assets.ErrInvalidUpload
	}
	return copy, nil
}
