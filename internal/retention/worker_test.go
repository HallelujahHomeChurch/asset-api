package retention

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRetentionSweepPreflightIsReadOnlyAndBounded(t *testing.T) {
	now := time.Date(2026, 8, 19, 19, 0, 0, 0, time.UTC)
	repository := &repositoryStub{
		candidates: candidates(101),
		preview:    []Preview{{CollectionID: "opaque-a", CandidateCount: 101, TotalBytes: 5050}},
	}
	worker := NewWorker(repository, false)

	result, err := worker.SweepExpiredCollectionItems(context.Background(), now, 7)
	if err != nil {
		t.Fatal(err)
	}
	if result.Scanned != 100 || result.Deleted != 0 || result.RemainingBacklog != 101 {
		t.Fatalf("result = %+v", result)
	}
	if repository.deleteCalls != 0 {
		t.Fatalf("delete calls = %d", repository.deleteCalls)
	}
	if repository.listLimits[0] != 100 || !repository.listTimes[0].Equal(now) {
		t.Fatalf("list limit/time = %d/%s", repository.listLimits[0], repository.listTimes[0])
	}
	if len(result.Preview) != 1 || result.Preview[0] != repository.preview[0] {
		t.Fatalf("preview = %+v", result.Preview)
	}
}

func TestRetentionSweepApplyDeletesRepeatedBatchesAndRetryIsIdempotent(t *testing.T) {
	now := time.Date(2026, 8, 19, 19, 0, 0, 0, time.UTC)
	repository := &repositoryStub{candidates: candidates(101)}
	worker := NewWorker(repository, true)

	result, err := worker.SweepExpiredCollectionItems(context.Background(), now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if result.Scanned != 101 || result.Deleted != 101 || result.RemainingBacklog != 0 {
		t.Fatalf("result = %+v", result)
	}
	if len(repository.deleteSizes) != 2 || repository.deleteSizes[0] != 100 || repository.deleteSizes[1] != 1 {
		t.Fatalf("delete sizes = %v", repository.deleteSizes)
	}

	retry, err := worker.SweepExpiredCollectionItems(context.Background(), now, 100)
	if err != nil || retry.Scanned != 0 || retry.Deleted != 0 {
		t.Fatalf("retry = %+v, %v", retry, err)
	}
}

func TestRetentionSweepApplyReportsConcurrentSkips(t *testing.T) {
	now := time.Date(2026, 8, 19, 19, 0, 0, 0, time.UTC)
	repository := &repositoryStub{
		candidates: candidates(3),
		deleteResult: DeleteResult{
			Deleted:        1,
			ExemptSkipped:  1,
			AlreadyRemoved: 1,
		},
	}
	result, err := NewWorker(repository, true).SweepExpiredCollectionItems(context.Background(), now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 || result.ExemptSkipped != 1 || result.AlreadyRemoved != 1 {
		t.Fatalf("result = %+v", result)
	}
}

func TestRetentionSweepApplyReturnsFailedBatchForSafeJobRetry(t *testing.T) {
	now := time.Date(2026, 8, 19, 19, 0, 0, 0, time.UTC)
	repository := &repositoryStub{candidates: candidates(2), deleteErr: errors.New("database unavailable")}

	result, err := NewWorker(repository, true).SweepExpiredCollectionItems(context.Background(), now, 100)
	if err == nil {
		t.Fatal("expected error")
	}
	if result.FailedBatches != 1 || result.FailedItems != 2 || result.RemainingBacklog != 2 {
		t.Fatalf("result = %+v", result)
	}
}

type repositoryStub struct {
	candidates   []Candidate
	preview      []Preview
	deleteResult DeleteResult
	deleteErr    error
	deleteCalls  int
	deleteSizes  []int
	listLimits   []int
	listTimes    []time.Time
}

func (r *repositoryStub) ListExpiredCollectionItems(_ context.Context, now time.Time, limit int) ([]Candidate, error) {
	r.listLimits = append(r.listLimits, limit)
	r.listTimes = append(r.listTimes, now)
	if len(r.candidates) < limit {
		limit = len(r.candidates)
	}
	return append([]Candidate(nil), r.candidates[:limit]...), nil
}

func (r *repositoryStub) PreviewExpiredCollectionItems(context.Context, time.Time) ([]Preview, error) {
	if r.preview != nil {
		return append([]Preview(nil), r.preview...), nil
	}
	return []Preview{{CollectionID: "opaque-a", CandidateCount: int64(len(r.candidates))}}, nil
}

func (r *repositoryStub) DeleteExpiredCollectionItems(_ context.Context, _ string, itemIDs []string, _ time.Time) (DeleteResult, error) {
	r.deleteCalls++
	r.deleteSizes = append(r.deleteSizes, len(itemIDs))
	if r.deleteErr != nil {
		return DeleteResult{}, r.deleteErr
	}
	result := r.deleteResult
	if result == (DeleteResult{}) {
		result.Deleted = len(itemIDs)
	}
	deleted := result.Deleted + result.ExemptSkipped + result.AlreadyRemoved
	if deleted > len(r.candidates) {
		deleted = len(r.candidates)
	}
	r.candidates = r.candidates[deleted:]
	return result, nil
}

func candidates(count int) []Candidate {
	result := make([]Candidate, count)
	for index := range result {
		result[index] = Candidate{CollectionID: "opaque-a", ItemID: fmt.Sprintf("item-%03d", index)}
	}
	return result
}
