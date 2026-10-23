package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/diagnostics"
)

func TestGrantDiagnosticsSeparateAuthorityWaitFromTransaction(t *testing.T) {
	_, store, objects, c, _ := broadcastRevocationFixture(t)
	paused := &pausedGrantAuthority{objects, make(chan struct{}, 1), make(chan struct{})}
	store.WithBroadcastObjects(paused)
	diagnostics.Default.Snapshot()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, err := store.WithCaptureGrant(ctx, c.ID, func(*assets.RecordingCapture, assets.RecordingBroadcastReader) error {
		t.Fatal("callback must not run before authority returns")
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("authority did not time out: %v", err)
	}
	rows := diagnostics.Default.Snapshot()
	if rows[diagnostics.GrantSQL][0] != 1 || rows[diagnostics.GrantAuthority][0] != 1 || rows[diagnostics.GrantAuthority][2] != 1 || rows[diagnostics.GrantAuthority][4] < 40 || rows[diagnostics.CaptureBegin][0] != 0 {
		t.Fatalf("wrong stage attribution: %v", rows)
	}
	store.WithBroadcastObjects(objects)
	_, err = store.WithCaptureGrant(context.Background(), c.ID, func(_ *assets.RecordingCapture, r assets.RecordingBroadcastReader) error {
		_, err := r.GetBroadcastProjection(context.Background(), c.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	rows = diagnostics.Default.Snapshot()
	for _, stage := range []diagnostics.Stage{diagnostics.CaptureBegin, diagnostics.CaptureOwnerLock, diagnostics.CaptureRow, diagnostics.CaptureLiveRow, diagnostics.CaptureCallback, diagnostics.CaptureCommit, diagnostics.ProjectionSQL, diagnostics.ProjectionReuse} {
		if rows[stage][0] != 1 {
			t.Fatalf("stage %d not reached: %v", stage, rows)
		}
	}
	if rows[diagnostics.ProjectionAuthority][0] != 0 {
		t.Fatal("prefetched authority was fetched twice")
	}
}
