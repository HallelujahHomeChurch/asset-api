package postgres

import (
	"context"
	"errors"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
	"strings"
	"testing"
	"time"
)

func TestLivePreviewFencingFailurePriorityAndCleanup(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	cap := assets.RecordingCapture{ID: strings.Repeat("f", 32), ActorID: "actor", RecordingID: "recording", CreateKey: "preview", State: "uploading", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}
	captures := NewRecordingCaptureStore(db)
	if _, err := captures.CreateCapture(ctx, cap); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE recording_live SET segments='[{"sequence":0,"renditions":{"480p":{"start":0,"end":30}}}]',revision=1,published_revision=1,published_sequence=0 WHERE capture_id=$1`, cap.ID)
	repo := NewRecordingPackageStore(db)
	c, err := repo.ClaimLivePreview(ctx)
	if err != nil || c.Last != 0 || c.Completed != -1 {
		t.Fatal(c, err)
	}
	if _, err = repo.ClaimLivePreview(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatal("duplicate", err)
	}
	if err = repo.FinishLivePreview(ctx, c, "", nil); err != nil {
		t.Fatal(err)
	}
	last, _, err := repo.LivePreviewProgress(ctx, cap.ID)
	if err != nil || last != -1 {
		t.Fatal("failure advanced", last, err)
	}
	exec(`UPDATE recording_live_previews SET next_attempt_at=now() WHERE capture_id=$1`, cap.ID)
	c, err = repo.ClaimLivePreview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stale := c
	stale.ClaimID = "stale"
	called := false
	publish := func(context.Context) error { called = true; return nil }
	if err = repo.FinishLivePreview(ctx, stale, "WEBVTT\n\n", publish); !errors.Is(err, assets.ErrConflict) || called {
		t.Fatal("stale publication", err)
	}
	if err = repo.FinishLivePreview(ctx, c, "WEBVTT\n\n", publish); err != nil || !called {
		t.Fatal(err)
	}
	last, _, err = repo.LivePreviewProgress(ctx, cap.ID)
	if err != nil || last != 0 {
		t.Fatal(last, err)
	}
	exec(`UPDATE recording_live_previews SET completed_sequence=-1 WHERE capture_id=$1`, cap.ID)
	// One uploaded rendition is not runnable validation and must not starve the existing DVR.
	if _, err = captures.UpdateCapture(ctx, cap.ID, func(c *assets.RecordingCapture) error {
		c.Objects = []assets.RecordingCaptureObject{{RecordingPackageObject: assets.RecordingPackageObject{Path: "480p/seg-000001.m4s", SizeBytes: 10, SHA256: strings.Repeat("a", 64)}, State: "queued"}}
		c.DeclaredObjects = 1
		c.DeclaredBytes = 10
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	partial, err := repo.ClaimLivePreview(ctx)
	if err != nil {
		t.Fatal("partial intake starved previews", err)
	}
	if err = repo.FinishLivePreview(ctx, partial, "", nil); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE recording_live_previews SET next_attempt_at=now() WHERE capture_id=$1`, cap.ID)
	if _, err = captures.UpdateCapture(ctx, cap.ID, func(c *assets.RecordingCapture) error {
		for _, path := range []string{"480p/init.mp4", "720p/init.mp4", "1080p/init.mp4", "720p/seg-000001.m4s", "1080p/seg-000001.m4s"} {
			c.Objects = append(c.Objects, assets.RecordingCaptureObject{RecordingPackageObject: assets.RecordingPackageObject{Path: path, SizeBytes: 10, SHA256: strings.Repeat("a", 64)}, State: "queued"})
		}
		c.DeclaredObjects = 6
		c.DeclaredBytes = 60
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.ClaimLivePreview(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatal("complete live intake lost priority", err)
	}
	exec(`UPDATE recording_capture_objects SET state='declared' WHERE capture_id=$1`, cap.ID)
	p := testRecordingPackage(t, "waiting", "waiting", "waiting")
	if err = repo.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err = repo.Freeze(ctx, p.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.ClaimLivePreview(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatal("optional work stole validation priority", err)
	}
	exec(`UPDATE recording_captures SET state='aborted',created_at=now()-interval '10 days',expires_at=now()-interval '9 days',terminal_at=now()-interval '8 days' WHERE id=$1`, cap.ID)
	var keys []string
	remove := func(_ context.Context, batch []string) error {
		if len(batch) > 1000 {
			t.Fatal("unbounded cleanup")
		}
		keys = append(keys, batch...)
		return nil
	}
	if err = captures.ReconcileLive(ctx, remove); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, key := range keys {
		if strings.HasSuffix(key, "previews/shared/seg-000000.jpg") {
			found = true
		}
	}
	if !found {
		t.Fatal("shared sprites leaked", keys)
	}
}

func TestLivePreviewRetentionFollowsVODWithoutHoldingStaging(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	captures := NewRecordingCaptureStore(db)
	cap := assets.RecordingCapture{ID: sealedLiveCapture(t, captures, 1)}
	packages := NewRecordingPackageStore(db)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE recording_packages SET state='ready',final_prefix='recordings/packages/'||id||'/final/ready/',ready_at=now(),media_expires_at=now()+interval '30 days' WHERE id=$1`, cap.ID)
	exec(`UPDATE recording_captures SET state='ready',package_id=$1,created_at=now()-interval '2 days',expires_at=now()-interval '1 day' WHERE id=$1`, cap.ID)
	exec(`UPDATE recording_live SET segments='[{"sequence":0,"renditions":{"480p":{"start":0,"end":30}}}]',revision=1,published_revision=1,published_sequence=0 WHERE capture_id=$1`, cap.ID)
	exec(`INSERT INTO recording_live_previews(capture_id,completed_sequence) VALUES($1,0)`, cap.ID)
	var keys []string
	remove := func(_ context.Context, batch []string) error { keys = append(keys, batch...); return nil }
	if err := captures.ReconcileLive(ctx, remove); err != nil || len(keys) > 0 {
		t.Fatal("deleted live sprites still used by VOD", err)
	}
	if err := packages.ReconcilePackages(ctx, remove); err != nil || len(keys) == 0 {
		t.Fatal("retention unnecessarily held staging", err)
	}
	keys = nil
	exec(`UPDATE recording_packages SET media_expires_at=now()-interval '2 hours' WHERE id=$1`, cap.ID)
	if err := captures.ReconcileLive(ctx, remove); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, key := range keys {
		if strings.HasSuffix(key, "previews/shared/seg-000000.jpg") {
			found = true
		}
	}
	if !found {
		t.Fatal("expired shared image not reclaimed", keys)
	}
}
