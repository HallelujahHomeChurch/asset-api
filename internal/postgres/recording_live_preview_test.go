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
	for _, policy := range []bool{false, true} {
		name := "legacy"
		if policy {
			name = "B1"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := isolatedIntegrationDB(t)
			if err := migrations.Run(ctx, db); err != nil {
				t.Fatal(err)
			}
			captures := NewRecordingCaptureStore(db)
			id := sealedLiveCapture(t, captures, 1)
			exec := func(q string, args ...any) {
				t.Helper()
				if _, err := db.ExecContext(ctx, q, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec(`UPDATE recording_packages SET state='ready',final_prefix='recordings/packages/'||id||'/final/ready/',ready_at=now(),media_expires_at=now()+interval '30 days' WHERE id=$1`, id)
			exec(`UPDATE recording_captures SET state='ready',package_id=$1,created_at=now()-interval '2 days',expires_at=now()-interval '1 day' WHERE id=$1`, id)
			exec(`UPDATE recording_live SET segments='[{"sequence":0,"renditions":{"480p":{"start":0,"end":30}}}]',revision=1,published_revision=1,published_sequence=0 WHERE capture_id=$1`, id)
			exec(`INSERT INTO recording_live_previews(capture_id,completed_sequence) VALUES($1,0)`, id)
			exec(`INSERT INTO recording_live_attempts(claim_id,capture_id,sequence) VALUES('temporary-live-claim',$1,0)`, id)
			// A final short segment can produce a VOD sprite beyond the live waterline.
			exec(`INSERT INTO recording_capture_objects(capture_id,path,size_bytes,sha256,state) SELECT capture_id,'480p/seg-000001.m4s',size_bytes,sha256,state FROM recording_capture_objects WHERE capture_id=$1 AND path='480p/seg-000000.m4s'`, id)
			if policy {
				exec(`INSERT INTO recording_broadcast_ranges(capture_id,recording_id,epoch,range_revision,start_sequence,end_sequence_exclusive,revoked,member_state) SELECT id,recording_id,1,1,0,1,false,'vod' FROM recording_captures WHERE id=$1`, id)
			}
			var keys []string
			remove := func(_ context.Context, batch []string) error { keys = append(keys, batch...); return nil }
			contains := func(key string) bool {
				for _, got := range keys {
					if got == key {
						return true
					}
				}
				return false
			}
			prefix := "recordings/captures/" + id + "/final/"
			// Failed deletion must remain retryable, without marking attempts purged.
			if err := captures.ReconcileLive(ctx, func(context.Context, []string) error { return errors.New("provider unavailable") }); err == nil {
				t.Fatal("cleanup failure lost")
			}
			var purged bool
			if err := db.QueryRowContext(ctx, `SELECT purged FROM recording_live_attempts WHERE capture_id=$1`, id).Scan(&purged); err != nil || purged {
				t.Fatal("failed cleanup marked purged", err)
			}
			exec(`UPDATE recording_live SET cleanup_after=now() WHERE capture_id=$1`, id)
			if err := captures.ReconcileLive(ctx, remove); err != nil {
				t.Fatal(err)
			}
			if !contains((assets.RecordingPackage{ID: id}).StagingKey("480p/seg-000000.m4s")) || !contains("recordings/packages/"+id+"/final/temporary-live-claim/480p/seg-000000.m4s") {
				t.Fatal("temporary media retained", keys)
			}
			for _, path := range []string{"480p/seg-000000.m4s", "480p/init.mp4", "current.json", "playlists/1/480p/index.m3u8"} {
				if contains(prefix+path) == policy {
					t.Fatal("canonical media retention differs from routing", path, keys)
				}
			}
			for _, key := range keys {
				if strings.Contains(key, "/previews/") {
					t.Fatal("deleted shared VOD preview", key)
				}
			}
			if err := db.QueryRowContext(ctx, `SELECT purged FROM recording_live_attempts WHERE capture_id=$1`, id).Scan(&purged); err != nil || !purged {
				t.Fatal("attempt not reclaimed", err)
			}
			var next time.Time
			if err := db.QueryRowContext(ctx, `SELECT cleanup_after FROM recording_live WHERE capture_id=$1`, id).Scan(&next); err != nil || !next.After(time.Now()) || next.After(time.Now().Add(25*time.Hour)) {
				t.Fatal("later cleanup not scheduled", next, err)
			}
			// An outstanding package grant extends retention beyond media expiry.
			exec(`UPDATE recording_packages SET media_expires_at=now()-interval '2 hours',grant_cleanup_after=now()+interval '1 hour' WHERE id=$1`, id)
			exec(`UPDATE recording_live SET cleanup_after=now() WHERE capture_id=$1`, id)
			keys = nil
			if err := captures.ReconcileLive(ctx, remove); err != nil {
				t.Fatal(err)
			}
			if contains(prefix+"previews/shared/seg-000000.jpg") || policy && contains(prefix+"current.json") {
				t.Fatal("active package grant lost media", keys)
			}
			// The subsequent scheduled sweep still deletes all retained objects.
			exec(`UPDATE recording_packages SET grant_cleanup_after=now()-interval '1 hour' WHERE id=$1`, id)
			exec(`UPDATE recording_live SET cleanup_after=now() WHERE capture_id=$1`, id)
			keys = nil
			if err := captures.ReconcileLive(ctx, remove); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"480p/seg-000000.m4s", "current.json", "playlists/1/480p/index.m3u8", "previews/current.json", "previews/shared/seg-000000.jpg", "previews/shared/seg-000001.jpg", "previews/shared/seg-000001.vtt", "previews/2/index.vtt"} {
				if !contains(prefix + path) {
					t.Fatal("expired capture object leaked", path, keys)
				}
			}
		})
	}
}
