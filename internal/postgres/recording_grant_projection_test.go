package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestLiveGrantDoesNotRewriteCaptureJournalOrLoadObjects(t *testing.T) {
	db, store, _, capture, _ := broadcastRevocationFixture(t)
	ctx := context.Background()
	var err error
	capture, err = store.UpdateCapture(ctx, capture.ID, func(c *assets.RecordingCapture) error {
		for i := 0; i < 900; i++ {
			key := fmt.Sprintf("history-%d", i)
			c.Receipts[key] = assets.RecordingCaptureStoredReceipt{Digest: "synthetic", Receipt: assets.RecordingCaptureReceipt{OperationKey: key, Operation: "confirm", CaptureID: c.ID, AcceptedAt: time.Now()}}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO recording_capture_objects(capture_id,path,size_bytes,sha256,state) SELECT $1,'1080p/seg-'||lpad(i::text,6,'0')||'.m4s',10,repeat('a',64),'queued' FROM generate_series(0,899) i`, capture.ID); err != nil {
		t.Fatal(err)
	}
	var before string
	if err = db.QueryRow(`SELECT xmin::text FROM recording_captures WHERE id=$1`, capture.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	until := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	_, err = store.WithCaptureGrant(ctx, capture.ID, func(c *assets.RecordingCapture, reader assets.RecordingBroadcastReader) error {
		if len(c.Receipts) != 0 || len(c.Objects) != 0 {
			t.Errorf("grant loaded producer history: receipts=%d objects=%d", len(c.Receipts), len(c.Objects))
		}
		if _, err := reader.GetBroadcastProjection(ctx, c.ID); err != nil {
			return err
		}
		c.ReadGrantUntil = &until
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var after string
	var receipts, objects int
	var saved time.Time
	if err = db.QueryRow(`SELECT xmin::text,(SELECT count(*) FROM jsonb_object_keys(receipts)),(SELECT count(*) FROM recording_capture_objects WHERE capture_id=$1) FROM recording_captures WHERE id=$1`, capture.ID).Scan(&after, &receipts, &objects); err != nil {
		t.Fatal(err)
	}
	if before != after || receipts != 900 || objects != 900 {
		t.Fatalf("grant rewrote capture: xmin %s -> %s, receipts=%d objects=%d", before, after, receipts, objects)
	}
	if err = db.QueryRow(`SELECT read_grant_until FROM recording_live WHERE capture_id=$1`, capture.ID).Scan(&saved); err != nil || !saved.Equal(until) {
		t.Fatalf("grant deadline not retained: %s %v", saved, err)
	}
	// Pointer mutation must not alias the previous deadline used for monotonicity.
	later := until.Add(time.Minute)
	_, err = store.WithCaptureGrant(ctx, capture.ID, func(c *assets.RecordingCapture, _ assets.RecordingBroadcastReader) error {
		*c.ReadGrantUntil = later
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT read_grant_until FROM recording_live WHERE capture_id=$1`, capture.ID).Scan(&saved); err != nil || !saved.Equal(later) {
		t.Fatalf("in-place deadline extension lost: %s %v", saved, err)
	}
	rollback := fmt.Errorf("synthetic signing failure")
	_, err = store.WithCaptureGrant(ctx, capture.ID, func(c *assets.RecordingCapture, _ assets.RecordingBroadcastReader) error {
		next := later.Add(time.Minute)
		c.ReadGrantUntil = &next
		return rollback
	})
	if err != rollback {
		t.Fatalf("signing failure changed: %v", err)
	}
	if err = db.QueryRow(`SELECT read_grant_until FROM recording_live WHERE capture_id=$1`, capture.ID).Scan(&saved); err != nil || !saved.Equal(later) {
		t.Fatalf("failed grant changed deadline: %s %v", saved, err)
	}

}
