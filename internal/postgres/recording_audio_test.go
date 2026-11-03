package postgres

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRecordingAudioClaimFencingPlanAndCleanup(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingPackageStore(db)
	ready := func(id string) {
		t.Helper()
		p := testRecordingPackage(t, id, id, id)
		if err := store.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		if err := store.Freeze(ctx, id, time.Now()); err != nil {
			t.Fatal(err)
		}
		c, err := store.ClaimPackageValidation(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.FinishPackageValidation(ctx, c.Package.ID, c.ClaimID, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	ready("audio-one")
	plan, err := store.PlanRecordingAudio(ctx)
	if err != nil || plan.Packages != 0 {
		t.Fatalf("plan %+v %v", plan, err)
	}
	c, err := store.ClaimPackageAudio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimPackageAudio(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("double claim: %v", err)
	}
	if err := store.FinishPackageAudio(ctx, c.Package.ID, "stale", true, func(context.Context) error { t.Fatal("stale published"); return nil }); !errors.Is(err, assets.ErrConflict) {
		t.Fatal(err)
	}
	if err := store.HeartbeatPackageAudio(ctx, c.Package.ID, c.ClaimID); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishPackageAudio(ctx, c.Package.ID, c.ClaimID, true, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	p, err := store.Get(ctx, c.Package.ID)
	if err != nil || p.Audio == nil || p.Audio.State != "ready" || p.Audio.PlaylistPath != "audio/aac-hls-v1/index.m3u8" {
		t.Fatalf("audio metadata %+v %v", p.Audio, err)
	}
	ready("audio-deleted")
	c, err = store.ClaimPackageAudio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewRecordingDeletionStore(db).DeleteRecording(ctx, c.Package.RecordingID); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishPackageAudio(ctx, c.Package.ID, c.ClaimID, true, func(context.Context) error { t.Fatal("deleted published"); return nil }); !errors.Is(err, assets.ErrConflict) {
		t.Fatal(err)
	}
	ready("audio-expired")
	c, err = store.ClaimPackageAudio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE recording_packages SET media_expires_at=now()-interval '2 hours',cleanup_after=now()-interval '1 day' WHERE id=$1`, c.Package.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.HeartbeatPackageAudio(ctx, c.Package.ID, c.ClaimID); !errors.Is(err, assets.ErrConflict) {
		t.Fatal(err)
	}
	var keys []string
	if err := store.CleanupPackage(ctx, c.Package.ID, func(_ context.Context, k []string) error { keys = append(keys, k...); return nil }); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(keys, "\n")
	if !strings.Contains(joined, "audio/aac-hls-v1/"+c.ClaimID+"/seg-000000.ts") || !strings.Contains(joined, "audio/aac-hls-v1/current.json") {
		t.Fatalf("audio not cleaned: %s", joined)
	}
	ready("historical")
	if _, err := db.Exec(`DELETE FROM recording_audio WHERE package_id='historical'`); err != nil {
		t.Fatal(err)
	}
	plan, err = store.PlanRecordingAudio(ctx)
	if err != nil || plan.Packages != 1 || plan.InputBytes <= 0 || plan.Segments != 1 {
		t.Fatalf("historical plan %+v %v", plan, err)
	}
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	plan2, err := store.PlanRecordingAudio(ctx)
	if err != nil || plan2 != plan {
		t.Fatalf("plan or migration enqueued history: %+v %v", plan2, err)
	}
}

func TestRecordingAudioRetryBudgetAndSourceFence(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingPackageStore(db)
	p := testRecordingPackage(t, "audio-retry", "audio-retry", "audio-retry")
	if err := store.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := store.Freeze(ctx, p.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	c, err := store.ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishPackageValidation(ctx, p.ID, c.ClaimID, true, ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		c, err = store.ClaimPackageAudio(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, err := db.Exec(`UPDATE recording_audio SET source_digest='changed' WHERE package_id=$1`, p.ID); err != nil {
				t.Fatal(err)
			}
			if err := store.FinishPackageAudio(ctx, p.ID, c.ClaimID, true, func(context.Context) error { t.Fatal("changed source published"); return nil }); !errors.Is(err, assets.ErrConflict) {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE recording_audio SET source_digest=(SELECT inventory->>'inventoryDigest' FROM recording_packages WHERE id=$1) WHERE package_id=$1`, p.ID); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.FinishPackageAudio(ctx, p.ID, c.ClaimID, false, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE recording_audio SET next_attempt_at=now()-interval '1 second' WHERE package_id=$1`, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ClaimPackageAudio(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("unbounded retry %v", err)
	}
	got, err := store.Get(ctx, p.ID)
	if err != nil || got.State != "ready" || got.Audio.State != "failed" || got.Audio.PlaylistPath != "" {
		t.Fatalf("video affected or failed audio playable %+v %v", got, err)
	}
}

func TestRecordingAudioPublicationSerializesBroadcastRevocation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, captures, objects, c, policy := broadcastRevocationFixture(t)
	packages := NewRecordingPackageStore(db)
	p := testRecordingPackage(t, c.ID, c.ActorID, "audio-publication-fixture")
	if err := packages.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := packages.Freeze(ctx, p.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	claim, err := packages.ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := packages.FinishPackageValidation(ctx, p.ID, claim.ClaimID, true, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE recording_packages SET recording_id=$2 WHERE id=$1`, p.ID, c.RecordingID); err != nil {
		t.Fatal(err)
	}
	claim, err = packages.ClaimPackageAudio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var schema string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = schema
	revokeDB := stdlib.OpenDB(*config)
	defer revokeDB.Close()
	revokeDB.SetMaxOpenConns(1)
	var pid int
	if err := revokeDB.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	revoker := NewRecordingCaptureStore(revokeDB).WithBroadcastObjects(objects)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	published, revoked := make(chan error, 1), make(chan error, 1)
	go func() {
		published <- packages.FinishPackageAudio(ctx, p.ID, claim.ClaimID, true, func(ctx context.Context) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	policy.RangeRevision++
	policy.Revoked = true
	policy.MemberState = "blocked"
	go func() { _, err := revoker.SetBroadcastRange(ctx, c.ID, policy); revoked <- err }()
	for {
		var waiting bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock' AND wait_event='advisory')`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-revoked:
			t.Fatalf("revoke crossed publication fence: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond * 10):
		}
	}
	unblock()
	if err := <-published; err != nil {
		t.Fatal(err)
	}
	if err := <-revoked; err != nil {
		t.Fatal(err)
	}
	got, err := captures.GetBroadcastRange(ctx, c.ID)
	if err != nil || !got.Revoked {
		t.Fatalf("revocation not converged %+v %v", got, err)
	}
	// The opposite ordering denies publication before its callback can run.
	if _, err := db.Exec(`UPDATE recording_audio SET state='pending',next_attempt_at=now(),attempts=0 WHERE package_id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := packages.ClaimPackageAudio(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("revoked parent claimed: %v", err)
	}
}
