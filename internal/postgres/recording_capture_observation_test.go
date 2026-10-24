package postgres

import (
	"context"
	"testing"
	"time"
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
