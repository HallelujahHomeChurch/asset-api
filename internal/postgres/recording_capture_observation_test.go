package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestCaptureObservationDoesNotWaitForProducerTransaction(t *testing.T) {
	db, store, _, c, _ := broadcastRevocationFixture(t)
	db.SetMaxOpenConns(2)
	ctx := context.Background()
	held, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Rollback()
	if _, err = held.ExecContext(ctx, `UPDATE recording_captures SET state='aborted' WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	readCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	observed, err := store.GetCapture(readCtx, c.ID)
	cancel()
	if err != nil || observed.State != "uploading" {
		t.Fatalf("status read waited for an uncommitted producer mutation: state=%s err=%v", observed.State, err)
	}
	if err = held.Commit(); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetCapture(ctx, c.ID)
	if err != nil || current.State != "aborted" {
		t.Fatalf("committed terminal state was lost: state=%s err=%v", current.State, err)
	}
}

func TestCaptureObservationPagesObjectsWithoutLoadingMutationReceipts(t *testing.T) {
	db, store, _, c, _ := broadcastRevocationFixture(t)
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	_, err := store.UpdateCapture(ctx, c.ID, func(current *assets.RecordingCapture) error {
		for _, path := range []string{"1080p/init.mp4", "1080p/seg-000000.m4s", "720p/init.mp4"} {
			current.Objects = append(current.Objects, assets.RecordingCaptureObject{RecordingPackageObject: assets.RecordingPackageObject{Path: path, SizeBytes: 10, SHA256: strings.Repeat("a", 64)}, State: "declared"})
		}
		current.Receipts["completed-confirm"] = assets.RecordingCaptureStoredReceipt{Digest: "digest", Receipt: assets.RecordingCaptureReceipt{OperationKey: "completed-confirm"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	observed, err := store.ObserveCapture(ctx, c.ID, "", 1)
	if err != nil || len(observed.Receipts) != 0 || len(observed.Objects) != 2 {
		t.Fatalf("unbounded observation: receipts=%d objects=%d err=%v", len(observed.Receipts), len(observed.Objects), err)
	}
	service := assets.NewRecordingCaptureService(store, nil, time.Now)
	first, err := service.Get(ctx, c.ID, c.ActorID, "", 1)
	if err != nil || len(first.Objects) != 1 || first.NextCursor != "1080p/init.mp4" {
		t.Fatalf("first page: %+v %v", first, err)
	}
	second, err := service.Get(ctx, c.ID, c.ActorID, first.NextCursor, 1)
	if err != nil || len(second.Objects) != 1 || second.Objects[0].Path != "1080p/seg-000000.m4s" || second.NextCursor == "" {
		t.Fatalf("second page: %+v %v", second, err)
	}
	last, err := service.Get(ctx, c.ID, c.ActorID, second.NextCursor, 1)
	if err != nil || len(last.Objects) != 1 || last.Objects[0].Path != "720p/init.mp4" || last.NextCursor != "" {
		t.Fatalf("last page: %+v %v", last, err)
	}
	if _, err = service.Get(ctx, c.ID, c.ActorID, "missing", 1); !errors.Is(err, assets.ErrInvalidInput) {
		t.Fatalf("missing cursor: %v", err)
	}
	if _, err = service.Get(ctx, c.ID, "other-actor", "", 1); !errors.Is(err, assets.ErrForbidden) {
		t.Fatalf("owner bypass: %v", err)
	}
	full, err := store.GetCapture(ctx, c.ID)
	if err != nil || len(full.Objects) != 3 || len(full.Receipts) != 1 {
		t.Fatalf("producer details lost: %v", err)
	}
}

func TestHealthyCaptureObservationDoesNotRewriteRows(t *testing.T) {
	db, store, _, c, _ := broadcastRevocationFixture(t)
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	var before, after string
	query := `SELECT c.xmin::text||':'||l.xmin::text FROM recording_captures c JOIN recording_live l ON l.capture_id=c.id WHERE c.id=$1`
	if err := db.QueryRowContext(ctx, query, c.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetCapture(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, query, c.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("observation rewrote capture/live rows: before=%s after=%s", before, after)
	}
}
