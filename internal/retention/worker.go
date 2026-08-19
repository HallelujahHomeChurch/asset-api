package retention

import (
	"context"
	"errors"
	"time"
)

const BatchSize = 100

type Candidate struct {
	CollectionID string
	ItemID       string
}

type Preview struct {
	CollectionID   string `json:"collectionId"`
	CandidateCount int64  `json:"candidateCount"`
	TotalBytes     int64  `json:"totalBytes"`
}

type DeleteResult struct {
	Deleted        int
	ExemptSkipped  int
	AlreadyRemoved int
}

type Result struct {
	Scanned          int
	Deleted          int
	ExemptSkipped    int
	AlreadyRemoved   int
	FailedItems      int
	FailedBatches    int
	RemainingBacklog int64
	Preview          []Preview
}

type Repository interface {
	ListExpiredCollectionItems(context.Context, time.Time, int) ([]Candidate, error)
	PreviewExpiredCollectionItems(context.Context, time.Time) ([]Preview, error)
	DeleteExpiredCollectionItems(context.Context, string, []string, time.Time) (DeleteResult, error)
}

type Worker struct {
	repository   Repository
	applyEnabled bool
}

func NewWorker(repository Repository, applyEnabled bool) *Worker {
	return &Worker{repository: repository, applyEnabled: applyEnabled}
}

func (w *Worker) SweepExpiredCollectionItems(ctx context.Context, now time.Time, _ int) (Result, error) {
	now = now.UTC()
	var result Result
	for {
		candidates, err := w.repository.ListExpiredCollectionItems(ctx, now, BatchSize)
		if err != nil {
			return w.finish(ctx, now, result, err)
		}
		result.Scanned += len(candidates)
		if len(candidates) == 0 || !w.applyEnabled {
			return w.finish(ctx, now, result, nil)
		}

		groups := make(map[string][]string)
		order := make([]string, 0)
		for _, candidate := range candidates {
			if _, ok := groups[candidate.CollectionID]; !ok {
				order = append(order, candidate.CollectionID)
			}
			groups[candidate.CollectionID] = append(groups[candidate.CollectionID], candidate.ItemID)
		}
		var batchErrors []error
		for _, collectionID := range order {
			itemIDs := groups[collectionID]
			deleted, err := w.repository.DeleteExpiredCollectionItems(ctx, collectionID, itemIDs, now)
			if err != nil {
				result.FailedBatches++
				result.FailedItems += len(itemIDs)
				batchErrors = append(batchErrors, err)
				continue
			}
			result.Deleted += deleted.Deleted
			result.ExemptSkipped += deleted.ExemptSkipped
			result.AlreadyRemoved += deleted.AlreadyRemoved
		}
		if len(batchErrors) > 0 {
			return w.finish(ctx, now, result, errors.Join(batchErrors...))
		}
	}
}

func (w *Worker) finish(ctx context.Context, now time.Time, result Result, sweepErr error) (Result, error) {
	preview, err := w.repository.PreviewExpiredCollectionItems(ctx, now)
	result.Preview = preview
	for _, value := range preview {
		result.RemainingBacklog += value.CandidateCount
	}
	return result, errors.Join(sweepErr, err)
}
