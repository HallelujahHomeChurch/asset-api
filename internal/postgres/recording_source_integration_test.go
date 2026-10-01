package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
)

type sourceBlobFixture struct{ commits int }

func (s *sourceBlobFixture) RecordingSourceBlocks(context.Context, string) (assets.RecordingSourceBlockList, error) {
	first, _ := assets.RecordingSourceBlockID(1)
	last, _ := assets.RecordingSourceBlockID(1789)
	return assets.RecordingSourceBlockList{Uncommitted: []assets.RecordingSourceBlock{{ID: first, SizeBytes: assets.RecordingSourceBlockBytes}, {ID: last, SizeBytes: 30_000_000_000 - 1788*assets.RecordingSourceBlockBytes}}}, nil
}

func (s *sourceBlobFixture) SignRecordingSourceBlock(_ context.Context, _ string, _ int, at time.Time) (assets.UploadTarget, error) {
	return assets.UploadTarget{Method: "PUT", URL: "https://source.invalid", ExpiresAt: at}, nil
}
func (s *sourceBlobFixture) CommitRecordingSource(_ context.Context, _ string, size int64) (assets.BlobMetadata, error) {
	s.commits++
	return assets.BlobMetadata{Size: size, ETag: "committed-etag"}, nil
}

func TestSourceCompleteIsDurableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingSourceStore(db)
	blob := &sourceBlobFixture{}
	now := time.Now().UTC().Truncate(time.Microsecond)
	svc := assets.NewRecordingSourceService(store, blob, func() time.Time { return now })
	input := assets.CreateRecordingSourceInput{ActorID: "admin-a", RecordingID: "recording-a", FileName: "聚會.mp4", SizeBytes: 30_000_000_000, ChecksumSHA256: strings.Repeat("a", 64), IdempotencyKey: "source-operation"}
	source, err := svc.Create(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if source.State != "uploading" || source.BlockCount <= 1000 {
		t.Fatalf("source: %+v", source)
	}
	page, err := svc.Status(ctx, source.ID, input.ActorID, 0, 1)
	if err != nil || len(page.ConfirmedBlocks) != 1 || page.ConfirmedBlocks[0] != 1 || page.NextCursor != 1 {
		t.Fatalf("resume page: %+v %v", page, err)
	}
	page, err = svc.Status(ctx, source.ID, input.ActorID, page.NextCursor, 1)
	if err != nil || len(page.ConfirmedBlocks) != 1 || page.ConfirmedBlocks[0] != source.BlockCount || page.NextCursor != 0 {
		t.Fatalf("last resume page: %+v %v", page, err)
	}
	if replay, err := svc.Create(ctx, input); err != nil || replay.ID != source.ID {
		t.Fatalf("create replay: %+v %v", replay, err)
	}
	wrong := input
	wrong.ActorID = "other"
	if _, err := svc.Create(ctx, wrong); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("wrong owner replay: %v", err)
	}
	if _, err := svc.Sign(ctx, source.ID, "other", []int{1}); !errors.Is(err, assets.ErrForbidden) {
		t.Fatalf("cross-owner: %v", err)
	}
	if _, err := svc.Sign(ctx, source.ID, input.ActorID, []int{source.BlockCount + 1}); !errors.Is(err, assets.ErrInvalidInput) {
		t.Fatalf("invalid block: %v", err)
	}
	if _, err := svc.Sign(ctx, source.ID, input.ActorID, []int{1, 1}); !errors.Is(err, assets.ErrInvalidInput) {
		t.Fatalf("duplicate: %v", err)
	}
	signed, err := svc.Sign(ctx, source.ID, input.ActorID, []int{1, source.BlockCount})
	if err != nil || len(signed) != 2 || signed[1].Number != source.BlockCount || signed[0].ExpiresAt.After(now.Add(15*time.Minute)) {
		t.Fatalf("sign: %+v %v", signed, err)
	}
	// The lock must reuse its connection instead of deadlocking a one-slot pool.
	db.SetMaxOpenConns(1)
	deadline, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	completed, err := svc.Complete(deadline, source.ID, input.ActorID)
	if err != nil || completed.State != "finalizing" || completed.RetryUntil == nil || !completed.RetryUntil.Equal(now.Add(7*24*time.Hour)) {
		t.Fatalf("complete: %+v %v", completed, err)
	}
	// Recreate both store and service to model a lost HTTP response/process restart.
	now = now.Add(25 * time.Hour)
	svc = assets.NewRecordingSourceService(NewRecordingSourceStore(db), blob, func() time.Time { return now })
	replay, err := svc.Complete(ctx, source.ID, input.ActorID)
	if err != nil || replay.State != "finalizing" || blob.commits != 1 || !replay.RetryUntil.Equal(*completed.RetryUntil) || replay.StagingETag != "committed-etag" {
		t.Fatalf("durable replay: %+v %v commits=%d", replay, err, blob.commits)
	}
	if _, err := svc.Sign(ctx, source.ID, input.ActorID, []int{1}); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("signed after complete: %v", err)
	}
}

func TestSourceActorCapAndExpiryAreAtomic(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingSourceStore(db)
	now := time.Now().UTC()
	svc := assets.NewRecordingSourceService(store, &sourceBlobFixture{}, func() time.Time { return now })
	var group sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := svc.Create(ctx, assets.CreateRecordingSourceInput{ActorID: "same-admin", RecordingID: fmt.Sprintf("recording-%d", i), FileName: "source.mp4", SizeBytes: 50_000_000_000, ChecksumSHA256: strings.Repeat("a", 64), IdempotencyKey: fmt.Sprintf("source-%d", i)})
			results <- err
		}()
	}
	group.Wait()
	close(results)
	created := 0
	for err := range results {
		if err == nil {
			created++
		} else if !errors.Is(err, assets.ErrConflict) {
			t.Fatal(err)
		}
	}
	if created != 1 {
		t.Fatalf("created %d active sources", created)
	}
	var id string
	if err := db.QueryRowContext(ctx, `SELECT id FROM recording_sources`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	now = now.Add(24 * time.Hour)
	if _, err := svc.Complete(ctx, id, "same-admin"); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("accepted expired source: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_sources SET size_bytes=50000000001 WHERE id=$1`, id); err == nil {
		t.Fatal("database accepted oversized source")
	}
}
