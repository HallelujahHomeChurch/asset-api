package lifecycle

import (
	"context"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestWorkerPurgesEveryCandidateObjectAndCompletes(t *testing.T) {
	repository := &repositoryStub{candidate: Candidate{
		AssetID: "asset-1",
		Keys:    []string{"staging", "original", "small", "medium", "large"},
	}}
	blobs := &blobStub{failures: map[string]int{}}
	worker := NewWorker(repository, blobs)
	worker.now = func() time.Time { return time.Date(2026, 7, 26, 0, 0, 0, 0, time.UTC) }

	processed, err := worker.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("processed=%v err=%v", processed, err)
	}
	if !repository.completed {
		t.Fatal("purge was not completed")
	}
	if len(blobs.deleted) != len(repository.candidate.Keys) {
		t.Fatalf("deleted=%v", blobs.deleted)
	}
}

func TestWorkerRetriesBlobFailure(t *testing.T) {
	repository := &repositoryStub{candidate: Candidate{AssetID: "asset-1", Keys: []string{"original"}}}
	blobs := &blobStub{failures: map[string]int{"original": 1}}
	worker := NewWorker(repository, blobs)

	processed, err := worker.ProcessOne(context.Background())
	if err == nil || !processed {
		t.Fatalf("processed=%v err=%v", processed, err)
	}
	if repository.retry == "" || repository.completed {
		t.Fatalf("retry=%q completed=%v", repository.retry, repository.completed)
	}
}

type repositoryStub struct {
	candidate Candidate
	completed bool
	retry     string
}

func (r *repositoryStub) ClaimPurge(context.Context, time.Time, time.Duration) (Candidate, bool, error) {
	return r.candidate, true, nil
}
func (r *repositoryStub) CompletePurge(context.Context, string, time.Time) error {
	r.completed = true
	return nil
}
func (r *repositoryStub) RetryPurge(_ context.Context, _ string, details string, _, _ time.Time) error {
	r.retry = details
	return nil
}

type blobStub struct {
	deleted  []string
	failures map[string]int
}

func (b *blobStub) Delete(_ context.Context, key string) error {
	if b.failures[key] > 0 {
		b.failures[key]--
		return assets.ErrInvalidUpload
	}
	b.deleted = append(b.deleted, key)
	return nil
}
