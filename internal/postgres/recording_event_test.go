package postgres

import (
	"context"
	"errors"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
	"hhc/asset-api/internal/recordingqueue"
	"sync"
	"testing"
	"time"
)

func TestRecordingEventIdleAndAdmissionFence(t *testing.T) {
	db := isolatedIntegrationDB(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := NewRecordingEventStore(db, true)
	if _, ok, err := s.ClaimDispatch(ctx); err != nil || ok {
		t.Fatalf("idle dispatched: %v %v", ok, err)
	}
	// Isolate admission from media readiness; real predicate coverage is separate.
	s.ready = func(context.Context, string) (bool, error) { return true, nil }
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	first, ok, err := s.ClaimDispatch(ctx)
	if err != nil || !ok {
		t.Fatalf("first %v %v", ok, err)
	}
	second, ok, err := s.ClaimDispatch(ctx)
	if err != nil || !ok || first.Lane == second.Lane {
		t.Fatalf("second %v %v", ok, err)
	}
	if _, ok, err = s.ClaimDispatch(ctx); err != nil || ok {
		t.Fatalf("third admitted: %v %v", ok, err)
	}
	if ok, err = s.Begin(ctx, first); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err = s.Begin(ctx, first); err != nil || ok {
		t.Fatal("duplicate began", ok, err)
	}
	forged := first
	forged.Generation++
	if err = s.Renew(ctx, forged); err == nil {
		t.Fatal("wrong generation renewed")
	}
	if _, err = db.Exec(`UPDATE recording_event_wakes SET generation=generation+1 WHERE lane=$1`, first.Lane); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, first, true); err != nil {
		t.Fatal(err)
	}
	if err = s.Renew(ctx, first); err == nil {
		t.Fatal("finished owner renewed")
	}
	next, ok, err := s.ClaimDispatch(ctx)
	if err != nil || !ok {
		t.Fatal("new generation lost", ok, err)
	}
	if next.Lane != first.Lane || next.Token == first.Token || next.Generation <= first.Generation {
		t.Fatalf("bad next: %+v", next)
	}
}
func TestRecordingEventMissingWorkerRecoversWithoutHourlySweep(t *testing.T) {
	db := isolatedIntegrationDB(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	s := NewRecordingEventStore(db, true)
	s.ready = func(context.Context, string) (bool, error) { return true, nil }
	if err := s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	e, ok, err := s.ClaimDispatch(ctx)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE recording_event_wakes SET lease_until=now()-interval '1 second' WHERE lane=$1`, e.Lane); err != nil {
		t.Fatal(err)
	}
	next, ok, err := s.ClaimDispatch(ctx)
	if err != nil || !ok || next.Token == e.Token {
		t.Fatal("expired dispatch did not recover", ok, err)
	}
	if active, err := s.Begin(ctx, e); err != nil || active {
		t.Fatal("stale queue message activated", active, err)
	}
}
func TestRecordingEventWakeCommitsWithWork(t *testing.T) {
	db := isolatedIntegrationDB(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	var before, after int64
	if err := db.QueryRow(`SELECT sum(generation) FROM recording_event_wakes`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE recording_packages SET state=state WHERE false`); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	if err = db.QueryRow(`SELECT sum(generation) FROM recording_event_wakes`).Scan(&after); err != nil || after != before {
		t.Fatal("rollback leaked wake", after, before, err)
	}
	if _, err = db.Exec(`UPDATE recording_packages SET state=state WHERE false`); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT sum(generation) FROM recording_event_wakes`).Scan(&after); err != nil || after <= before {
		t.Fatal("work change did not wake", after, before, err)
	}
}

func TestRecordingEventPreviewUsesFreeSlotAlongsideClaimedValidation(t *testing.T) {
	db := isolatedIntegrationDB(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	packages := NewRecordingPackageStore(db)
	pending := testRecordingPackage(t, "event-package", "event-actor", "event-recording")
	if err := packages.Create(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if err := packages.Freeze(ctx, pending.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	// No required work can be displaced; after it owns one slot, optional work may use the other.
	var priority bool
	if err := db.QueryRow(`SELECT ` + previewWorkWaiting).Scan(&priority); err != nil || priority {
		t.Fatal("required work lost priority", priority, err)
	}
	claim, err := packages.ClaimPackageValidation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT ` + previewWorkWaiting).Scan(&priority); err != nil || !priority {
		t.Fatal("active validation starved free slot", priority, err)
	}
	if claim.ClaimID == "" {
		t.Fatal("missing media claim")
	}
}

func TestRecordingEventExhaustedCrashSettlesWithoutHourlyMaintenance(t *testing.T) {
	db := isolatedIntegrationDB(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	packages := NewRecordingPackageStore(db)
	p := testRecordingPackage(t, "exhausted-event", "actor", "recording")
	if err := packages.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := packages.Freeze(ctx, p.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE recording_packages SET state='validating',validation_attempts=3,claimed_until=now()-interval '1 second' WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingEventStore(db, true)
	ready, err := store.workReady(ctx, "processing")
	if err != nil || !ready {
		t.Fatal("exhausted worker needs a settlement wake", ready, err)
	}
	if _, err := packages.ClaimPackageValidation(ctx); !errors.Is(err, assets.ErrNotFound) {
		t.Fatal(err)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM recording_packages WHERE id=$1`, p.ID).Scan(&state); err != nil || state != "failed" {
		t.Fatal(state, err)
	}
	ready, err = store.workReady(ctx, "processing")
	if err != nil || ready {
		t.Fatal("settled work woke again", ready, err)
	}
}

func TestRecordingEventMultipleDispatchersShareAdmission(t *testing.T) {
	db := isolatedIntegrationDB(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingEventStore(db, true)
	store.ready = func(context.Context, string) (bool, error) { return true, nil }
	var wg sync.WaitGroup
	events := make(chan recordingqueue.Event, 32)
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, ok, err := store.ClaimDispatch(ctx)
			if err != nil {
				errs <- err
			}
			if ok {
				events <- e
			}
		}()
	}
	wg.Wait()
	close(events)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(events) > 2 {
		t.Fatal("admission exceeded two", len(events))
	}
	for e := range events {
		active, err := store.Begin(ctx, e)
		if err != nil || !active {
			t.Fatal(active, err)
		}
		if err := store.Sent(ctx, e); err != nil {
			t.Fatal(err)
		}
		var state string
		if err := db.QueryRow(`SELECT state FROM recording_event_wakes WHERE lane=$1`, e.Lane).Scan(&state); err != nil || state != "running" {
			t.Fatal("late send acknowledgement downgraded worker", state, err)
		}
	}
}
