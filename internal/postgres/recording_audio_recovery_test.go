package postgres

import (
	"context"
	"errors"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
	"testing"
	"time"
)

func TestAudioRecoveryIsOneAttemptAndRejectsReplay(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := NewRecordingPackageStore(db)
	p := testRecordingPackage(t, "recovery", "actor", "recording")
	if err := s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.Freeze(ctx, p.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	c, err := s.ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishPackageValidation(ctx, p.ID, c.ClaimID, true, ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		c, err = s.ClaimPackageAudio(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.FinishPackageAudio(ctx, p.ID, c.ClaimID, false, nil); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`UPDATE recording_audio SET next_attempt_at=now()-interval '1 second' WHERE package_id=$1`, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := s.PlanRecordingAudioRetry(ctx, p.ID)
	if err != nil || plan.LastClaim != c.ClaimID || plan.Segments != 1 || plan.InputBytes <= 0 {
		t.Fatalf("plan %+v %v", plan, err)
	}
	if err = s.RetryRecordingAudio(ctx, p.ID, "stale"); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("stale preview: %v", err)
	}
	var expires time.Time
	if err = db.QueryRow(`SELECT media_expires_at FROM recording_packages WHERE id=$1`, p.ID).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE recording_packages SET media_expires_at=now()-interval '1 day' WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RetryRecordingAudio(ctx, p.ID, plan.LastClaim); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("expired apply: %v", err)
	}
	if _, err = db.Exec(`UPDATE recording_packages SET media_expires_at=$2 WHERE id=$1`, p.ID, expires); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- s.RetryRecordingAudio(ctx, p.ID, plan.LastClaim) }()
	}
	first, second := <-results, <-results
	if !((first == nil && errors.Is(second, assets.ErrConflict)) || (second == nil && errors.Is(first, assets.ErrConflict))) {
		t.Fatalf("concurrent apply: %v %v", first, second)
	}
	if err = s.RetryRecordingAudio(ctx, p.ID, plan.LastClaim); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("pending replay: %v", err)
	}
	c, err = s.ClaimPackageAudio(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishPackageAudio(ctx, p.ID, c.ClaimID, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimPackageAudio(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("more than one retry: %v", err)
	}
	if err = s.RetryRecordingAudio(ctx, p.ID, plan.LastClaim); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("failed replay: %v", err)
	}
	var attempts int
	if err = db.QueryRow(`SELECT count(*) FROM recording_audio_attempts WHERE package_id=$1`, p.ID).Scan(&attempts); err != nil || attempts != 4 {
		t.Fatalf("audit %d %v", attempts, err)
	}
	if _, err = db.Exec(`UPDATE recording_audio SET source_digest='changed' WHERE package_id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PlanRecordingAudioRetry(ctx, p.ID); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("changed source plan: %v", err)
	}
	if err = s.RetryRecordingAudio(ctx, p.ID, c.ClaimID); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("changed source apply: %v", err)
	}
	if _, err = db.Exec(`UPDATE recording_audio SET source_digest=(SELECT inventory->>'inventoryDigest' FROM recording_packages WHERE id=$1) WHERE package_id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err = NewRecordingDeletionStore(db).DeleteRecording(ctx, p.RecordingID); err != nil {
		t.Fatal(err)
	}
	if err = s.RetryRecordingAudio(ctx, p.ID, c.ClaimID); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("deleted apply: %v", err)
	}
}
