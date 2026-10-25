package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
)

func previewHandoffFixture(t *testing.T) (*sql.DB, *RecordingPackageStore, string) {
	t.Helper()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("c", 32)
	cap := assets.RecordingCapture{ID: id, ActorID: "actor", RecordingID: "handoff", CreateKey: "handoff", State: "uploading", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}
	if _, err := NewRecordingCaptureStore(db).CreateCapture(context.Background(), cap); err != nil {
		t.Fatal(err)
	}
	handoffExec(t, db, `UPDATE recording_live SET segments='[{"sequence":0,"renditions":{"480p":{"start":0,"end":10.01}}}]',revision=1,published_revision=1,published_sequence=0 WHERE capture_id=$1`, id)
	return db, NewRecordingPackageStore(db), id
}

func handoffExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

const handoffPackageReady = `UPDATE recording_packages SET state='ready',completed_at=now(),ready_at=now(),final_prefix='recordings/packages/'||id||'/final/handoff/',media_expires_at=now()+interval '30 days' WHERE id=$1`

func TestLivePreviewOwnershipIncludesSealAndAuxiliaryOnlyWork(t *testing.T) {
	for _, state := range []string{"uploading", "freezing", "validating"} {
		t.Run(state, func(t *testing.T) {
			db, repo, id := previewHandoffFixture(t)
			handoffExec(t, db, `UPDATE recording_captures SET state=$2 WHERE id=$1`, id, state)
			events := NewRecordingEventStore(db, false)
			for _, lane := range []string{"live", "preview"} {
				ready, err := events.workReady(context.Background(), lane)
				if err != nil || ready != (lane == "live") {
					t.Fatalf("%s wake=%v err=%v", lane, ready, err)
				}
			}
			claim, err := repo.ClaimLivePreview(context.Background())
			if err != nil || claim.Last != 0 {
				t.Fatal(claim, err)
			}
			if err := repo.FinishLivePreview(context.Background(), claim, "WEBVTT\n\n", func(context.Context) error { return nil }); err != nil {
				t.Fatal(err)
			}
			ready, err := events.workReady(context.Background(), "live")
			if err != nil || ready {
				t.Fatal("completed thumbnail keeps waking live", ready, err)
			}
		})
	}
}

func TestLiveToVODPreviewWaitsForLiveClaimAndRejectsStalePublication(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(map[bool]string{false: "finish", true: "expire"}[expire], func(t *testing.T) {
			db, repo, id := previewHandoffFixture(t)
			ctx := context.Background()
			live, err := repo.ClaimLivePreview(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.Create(ctx, testRecordingPackage(t, id, "actor", "sealed-package")); err != nil {
				t.Fatal(err)
			}
			handoffExec(t, db, handoffPackageReady, id)
			handoffExec(t, db, `UPDATE recording_captures SET state='ready',package_id=$1 WHERE id=$1`, id)
			if _, err := repo.ClaimPackagePreview(ctx); !errors.Is(err, assets.ErrNotFound) {
				t.Fatal("VOD overlapped valid live claim", err)
			}
			if expire {
				handoffExec(t, db, `UPDATE recording_live_previews SET claimed_until=now()-interval '1 second' WHERE capture_id=$1`, id)
				handoffExec(t, db, `UPDATE recording_processing_slots SET leased_until=now()-interval '1 second' WHERE claim_id=$1`, live.ClaimID)
			} else if err := repo.FinishLivePreview(ctx, live, "WEBVTT\n\n", func(context.Context) error { return nil }); err != nil {
				t.Fatal(err)
			}
			vod, err := repo.ClaimPackagePreview(ctx)
			if err != nil || vod.Package.ID != id {
				t.Fatal(vod, err)
			}
			if _, err := repo.ClaimLivePreview(ctx); !errors.Is(err, assets.ErrNotFound) {
				t.Fatal("live reclaimed a VOD thumbnail", err)
			}
			published := false
			err = repo.FinishLivePreview(ctx, live, "stale", func(context.Context) error { published = true; return nil })
			if !errors.Is(err, assets.ErrConflict) || published {
				t.Fatal("stale live claim published", err, published)
			}
			last, _, err := repo.LivePreviewProgress(ctx, id)
			want := 0
			if expire {
				want = -1
			}
			if err != nil || last != want {
				t.Fatal("handoff lost durable progress", last, err)
			}
		})
	}
}

func waitForHandoffLock(t *testing.T, db *sql.DB, done <-chan error) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		var waiting int
		if err := db.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND cardinality(pg_blocking_pids(pid))>0`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		select {
		case err := <-done:
			t.Fatal("claim did not serialize on preview progress", err)
		case <-deadline:
			t.Fatal("claim never reached progress lock")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A VOD admission snapshot can predate a concurrently committed live lease.
func TestPreviewHandoffRechecksAfterProgressLock(t *testing.T) {
	db, repo, id := previewHandoffFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := repo.Create(ctx, testRecordingPackage(t, id, "actor", "sealed-package")); err != nil {
		t.Fatal(err)
	}
	handoffExec(t, db, `UPDATE recording_captures SET package_id=$1 WHERE id=$1`, id)
	handoffExec(t, db, `INSERT INTO recording_live_previews(capture_id) VALUES($1)`, id)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM recording_live_previews WHERE capture_id=$1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	// Live uses SKIP LOCKED and must not compete with the progress owner.
	if _, err := repo.ClaimLivePreview(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatal(err)
	}
	handoffExec(t, db, handoffPackageReady, id)
	done := make(chan error, 1)
	go func() { _, err := repo.ClaimPackagePreview(ctx); done <- err }()
	waitForHandoffLock(t, db, done)
	if _, err := tx.ExecContext(ctx, `UPDATE recording_live_previews SET claim_id='old-live',claimed_until=now()+interval '1 minute' WHERE capture_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, assets.ErrNotFound) {
		t.Fatal("VOD used pre-lock live lease", err)
	}
	if _, err := repo.ClaimLivePreview(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatal("live reclaimed ready VOD", err)
	}
}

func TestLiveCoverAuxiliaryOnlyWakeAndExhaustedRecovery(t *testing.T) {
	db := isolatedIntegrationDB(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	covers := NewRecordingLiveCoverStore(db)
	cover, err := covers.Create(ctx, "defaults", "", "operator", "upload", "image/png", "digest", "custom")
	if err != nil {
		t.Fatal(err)
	}
	if err := covers.Queue(ctx, cover.ID); err != nil {
		t.Fatal(err)
	}
	events := NewRecordingEventStore(db, false)
	assertLane := func(want bool) {
		t.Helper()
		for _, lane := range []string{"live", "preview"} {
			ready, err := events.workReady(ctx, lane)
			if err != nil || ready != (lane == "live" && want) {
				t.Fatalf("%s ready=%v err=%v", lane, ready, err)
			}
		}
	}
	assertLane(true)
	claim, err := covers.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertLane(false)
	handoffExec(t, db, `UPDATE recording_live_covers SET attempts=3,claimed_until=now()-interval '1 second' WHERE id=$1`, cover.ID)
	handoffExec(t, db, `UPDATE recording_processing_slots SET leased_until=now()-interval '1 second' WHERE claim_id=$1`, claim.ClaimID)
	assertLane(true)
	if _, err := covers.Claim(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatal(err)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM recording_live_covers WHERE id=$1`, cover.ID).Scan(&state); err != nil || state != "failed" {
		t.Fatal(state, err)
	}
	assertLane(false)
}
