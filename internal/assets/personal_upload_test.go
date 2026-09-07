package assets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
	"time"
)

type personalUploadRepository struct{ *memoryRepository }

func (r *personalUploadRepository) PersonalUploadAssetID(_ context.Context, owner, upload string) (string, error) {
	for id, s := range r.sessions {
		if s.ID == upload {
			a := r.assets[id]
			if a.OwnerID == owner && a.Namespace == PersonalNamespace {
				return id, nil
			}
		}
	}
	return "", ErrNotFound
}

type personalUploadBlobs struct{ *memoryBlobStore }

func (b *personalUploadBlobs) PutOnce(ctx context.Context, key string, reader io.Reader, size int64, _ string) (BlobProperties, error) {
	if err := ctx.Err(); err != nil {
		return BlobProperties{}, err
	}
	if _, ok := b.objects[key]; ok {
		return BlobProperties{}, ErrConflict
	}
	data, err := io.ReadAll(io.LimitReader(reader, size+1))
	if err != nil {
		return BlobProperties{}, err
	}
	if int64(len(data)) != size {
		return BlobProperties{}, ErrInvalidUpload
	}
	b.objects[key] = data
	return b.Inspect(ctx, key, "", size)
}
func TestPersonalUploadOwnershipAndImmutableScanLifecycle(t *testing.T) {
	repo := &personalUploadRepository{newMemoryRepository()}
	blobs := &personalUploadBlobs{newMemoryBlobStore()}
	svc := NewService(repo, blobs, "", time.Now)
	ctx := context.Background()
	payload := []byte("%PDF-1.7\nprivate")
	upload, err := svc.CreatePersonalUpload(ctx, "alice", PersonalUploadInput{FileName: "private.pdf", MIMEType: "application/pdf", SizeBytes: int64(len(payload))}, "operation")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.PersonalUpload(ctx, "bob", upload.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign upload=%v", err)
	}
	if err = svc.PutPersonalUpload(ctx, "alice", upload.ID, int64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if err = svc.PutPersonalUpload(ctx, "alice", upload.ID, int64(len(payload)), bytes.NewReader(payload)); !errors.Is(err, ErrConflict) {
		t.Fatalf("rewrite=%v", err)
	}
	sum := sha256.Sum256(payload)
	state, err := svc.CompletePersonalUpload(ctx, "alice", upload.ID, CompleteUploadInput{SizeBytes: int64(len(payload)), MIMEType: "application/pdf", ChecksumSHA256: hex.EncodeToString(sum[:])})
	if err != nil || state.ScanStatus != ScanPending || state.UploadStatus != UploadCompleted {
		t.Fatalf("completion=%+v %v", state, err)
	}
	if err = svc.PutPersonalUpload(ctx, "alice", upload.ID, int64(len(payload)), bytes.NewReader(payload)); !errors.Is(err, ErrConflict) {
		t.Fatalf("completed rewrite=%v", err)
	}
	canceledUpload, err := svc.CreatePersonalUpload(ctx, "alice", PersonalUploadInput{FileName: "cancel.pdf", MIMEType: "application/pdf", SizeBytes: int64(len(payload))}, "canceled")
	if err != nil {
		t.Fatal(err)
	}
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err = svc.PutPersonalUpload(canceledCtx, "alice", canceledUpload.ID, int64(len(payload)), bytes.NewReader(payload)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write=%v", err)
	}
	canceledAsset, session, err := svc.personalUpload(ctx, "alice", canceledUpload.ID)
	if err != nil || canceledAsset.UploadStatus != UploadCreated {
		t.Fatalf("canceled state=%+v %v", canceledAsset, err)
	}
	if _, ok := blobs.objects[session.StagingObjectKey]; ok {
		t.Fatal("canceled bytes published")
	}

	if _, err = svc.CreatePersonalUpload(ctx, "alice", PersonalUploadInput{FileName: "huge.pdf", MIMEType: "application/pdf", SizeBytes: (200 << 20) + 1}, "huge"); err == nil {
		t.Fatal("accepted oversize")
	}
	if _, err = svc.CreatePersonalUpload(ctx, "alice", PersonalUploadInput{FileName: "wrong.exe", MIMEType: "application/pdf", SizeBytes: 1}, "wrong"); err == nil {
		t.Fatal("accepted MIME mismatch")
	}
}

func TestPersonalCompletionRejectsInvalidDeckGraph(t *testing.T) {
	repo := &personalUploadRepository{newMemoryRepository()}
	blobs := &personalUploadBlobs{newMemoryBlobStore()}
	service := NewService(repo, blobs, "", time.Now)
	payload := []byte(`{"schemaVersion":1,"id":"deck","name":"Deck","width":1920,"height":1080,"slideOrder":["missing"],"slides":{},"assets":{}}`)
	upload, err := service.CreatePersonalUpload(context.Background(), "alice", PersonalUploadInput{FileName: "deck.lpdeck", MIMEType: "application/vnd.hhc.presenter+json", SizeBytes: int64(len(payload))}, "deck")
	if err != nil {
		t.Fatal(err)
	}
	if err = service.PutPersonalUpload(context.Background(), "alice", upload.ID, int64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	checksum := sha256.Sum256(payload)
	_, err = service.CompletePersonalUpload(context.Background(), "alice", upload.ID, CompleteUploadInput{SizeBytes: int64(len(payload)), MIMEType: "application/vnd.hhc.presenter+json", ChecksumSHA256: hex.EncodeToString(checksum[:])})
	if !errors.Is(err, ErrInvalidUpload) {
		t.Fatalf("invalid graph=%v", err)
	}
	if len(repo.scanRequests) != 0 {
		t.Fatal("invalid deck queued for scanning")
	}
}

type completedDuringPut struct {
	*personalUploadBlobs
	repo *personalUploadRepository
}

func (b *completedDuringPut) PutOnce(ctx context.Context, key string, reader io.Reader, size int64, mime string) (BlobProperties, error) {
	props, err := b.personalUploadBlobs.PutOnce(ctx, key, reader, size, mime)
	if err != nil {
		return props, err
	}
	for id, session := range b.repo.sessions {
		if session.StagingObjectKey == key {
			session.Status = UploadCompleted
			b.repo.sessions[id] = session
		}
	}
	return props, nil
}
func TestPersonalLatePutRemovesObsoleteStaging(t *testing.T) {
	repo := &personalUploadRepository{newMemoryRepository()}
	blobs := &completedDuringPut{personalUploadBlobs: &personalUploadBlobs{newMemoryBlobStore()}, repo: repo}
	service := NewService(repo, blobs, "", time.Now)
	upload, err := service.CreatePersonalUpload(context.Background(), "alice", PersonalUploadInput{FileName: "file.pdf", MIMEType: "application/pdf", SizeBytes: 4}, "late")
	if err != nil {
		t.Fatal(err)
	}
	if err = service.PutPersonalUpload(context.Background(), "alice", upload.ID, 4, bytes.NewBufferString("%PDF")); !errors.Is(err, ErrConflict) {
		t.Fatalf("late write=%v", err)
	}
	if len(blobs.objects) != 0 {
		t.Fatal("late staging retained")
	}
}
