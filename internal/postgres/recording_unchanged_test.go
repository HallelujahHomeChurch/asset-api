package postgres

import (
	"context"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestCaptureStatusReplayDoesNotRewriteRows(t *testing.T) {
	db, store, _, capture, _ := broadcastRevocationFixture(t)
	ctx := context.Background()
	noop := func(*assets.RecordingCapture) error { return nil }
	if _, err := store.UpdateCapture(ctx, capture.ID, noop); err != nil {
		t.Fatal(err)
	}
	tuples := func() [2]string {
		t.Helper()
		var values [2]string
		if err := db.QueryRow(`SELECT c.xmin::text || ':' || c.ctid::text,l.xmin::text || ':' || l.ctid::text FROM recording_captures c JOIN recording_live l ON l.capture_id=c.id WHERE c.id=$1`, capture.ID).Scan(&values[0], &values[1]); err != nil {
			t.Fatal(err)
		}
		return values
	}
	before := tuples()
	if _, err := store.UpdateCapture(ctx, capture.ID, noop); err != nil {
		t.Fatal(err)
	}
	if after := tuples(); after != before {
		t.Errorf("unchanged status rewrote capture/live tuples: %v -> %v", before, after)
	}
	until := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	confirmed := until.Add(-time.Minute)
	if _, err := store.UpdateCapture(ctx, capture.ID, func(c *assets.RecordingCapture) error {
		c.ReadGrantUntil = &until
		c.LastMediaConfirmedAt = &confirmed
		c.Receipts["status-replay"] = assets.RecordingCaptureStoredReceipt{Digest: "synthetic", Receipt: assets.RecordingCaptureReceipt{OperationKey: "status-replay", Operation: "confirm", CaptureID: c.ID, AcceptedAt: confirmed}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var savedGrant, savedConfirmed time.Time
	var receipt bool
	if err := db.QueryRow(`SELECT l.read_grant_until,c.last_media_confirmed_at,c.receipts ? 'status-replay' FROM recording_captures c JOIN recording_live l ON l.capture_id=c.id WHERE c.id=$1`, capture.ID).Scan(&savedGrant, &savedConfirmed, &receipt); err != nil {
		t.Fatal(err)
	}
	if !savedGrant.Equal(until) || !savedConfirmed.Equal(confirmed) || !receipt {
		t.Fatal("changed capture or deadline not persisted")
	}
	before = tuples()
	if _, err := store.UpdateCapture(ctx, capture.ID, noop); err != nil {
		t.Fatal(err)
	}
	if after := tuples(); after != before {
		t.Errorf("replay with existing deadline rewrote tuples: %v -> %v", before, after)
	}
}
