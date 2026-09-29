package recordingvalidation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
)

type fakeRepository struct {
	claim  postgres.RecordingValidationClaim
	ready  bool
	failed bool
	finished bool
	lease  time.Duration
}

func (r *fakeRepository) ClaimValidation(_ context.Context, _ time.Time, lease time.Duration) (postgres.RecordingValidationClaim, error) {
	r.lease = lease
	return r.claim, nil
}
func (r *fakeRepository) MarkRecordingReady(_ context.Context, _, _ string, _ float64) error {
	r.ready = true
	return nil
}
func (r *fakeRepository) FinishRecordingValidation(_ context.Context, _, _ string, permanent bool, _ string, _ time.Time) error {
	r.finished = true
	r.failed = permanent
	return nil
}

func TestWorkerRejectsInvalidCodecWithoutRetry(t *testing.T) {
	repo := &fakeRepository{claim: postgres.RecordingValidationClaim{Session: assets.RecordingUploadSession{ID: "invalid-codec"}, ClaimID: "claim", Attempts: 1}}
	worker := New(repo, func(context.Context, string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(nil)), nil }, func(context.Context, string) ([]byte, error) {
		return []byte(`{"streams":[{"codec_type":"video","codec_name":"hevc"},{"codec_type":"audio","codec_name":"aac"}],"format":{"duration":"9000"}}`), nil
	}, time.Now)
	if err := worker.RunOnce(context.Background()); err != nil || !repo.finished || !repo.failed || repo.ready {
		t.Fatalf("invalid codec: finished=%v failed=%v ready=%v err=%v", repo.finished, repo.failed, repo.ready, err)
	}
}

func TestWorkerRetriesUncertainProbeFailure(t *testing.T) {
	repo := &fakeRepository{claim: postgres.RecordingValidationClaim{Session: assets.RecordingUploadSession{ID: "probe-failure"}, ClaimID: "claim", Attempts: 1}}
	worker := New(repo, nil, func(context.Context, string) ([]byte, error) { return nil, io.ErrUnexpectedEOF }, time.Now)
	if err := worker.RunOnce(context.Background()); err != nil || !repo.finished || repo.failed {
		t.Fatalf("uncertain probe: finished=%v failed=%v err=%v", repo.finished, repo.failed, err)
	}
}

func box(kind string, payload []byte) []byte {
	value := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(value, uint32(len(value)))
	copy(value[4:8], kind)
	copy(value[8:], payload)
	return value
}

func TestWorkerOnlyMarksValidatedBytesReady(t *testing.T) {
	data := bytes.Join([][]byte{box("ftyp", []byte("isom0000")), box("moov", []byte("meta")), box("mdat", []byte("bytes"))}, nil)
	sum := sha256.Sum256(data)
	repo := &fakeRepository{claim: postgres.RecordingValidationClaim{Session: assets.RecordingUploadSession{ID: "one", ObjectKey: "recordings/one.mp4", SizeBytes: int64(len(data)), ChecksumSHA256: hex.EncodeToString(sum[:])}, ClaimID: "claim", Attempts: 1}}
	worker := New(repo, func(context.Context, string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }, func(context.Context, string) ([]byte, error) {
		return []byte(`{"streams":[{"codec_type":"video","codec_name":"h264"},{"codec_type":"audio","codec_name":"aac"}],"format":{"duration":"9000"}}`), nil
	}, time.Now)
	if err := worker.RunOnce(context.Background()); err != nil || !repo.ready {
		t.Fatalf("ready=%v err=%v", repo.ready, err)
	}
	repo.ready = false
	repo.claim.Session.ChecksumSHA256 = hex.EncodeToString(make([]byte, 32))
	repo.claim.Attempts = 3
	if err := worker.RunOnce(context.Background()); err != nil || repo.ready || !repo.failed {
		t.Fatalf("ready=%v failed=%v err=%v", repo.ready, repo.failed, err)
	}
}

func TestWorkerBoundsValidationAttemptToFifteenMinutes(t *testing.T) {
	repo := &fakeRepository{claim: postgres.RecordingValidationClaim{Session: assets.RecordingUploadSession{ID: "one"}, ClaimID: "claim", Attempts: 1}}
	worker := New(repo, nil, func(ctx context.Context, _ string) ([]byte, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 15*time.Minute || time.Until(deadline) < 14*time.Minute {
			t.Fatalf("validation deadline = %s, want within 15 minutes", deadline)
		}
		return nil, context.DeadlineExceeded
	}, time.Now)
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.lease > 20*time.Minute || repo.lease < 15*time.Minute {
		t.Fatalf("claim lease = %s, want enough for one bounded attempt", repo.lease)
	}
}
