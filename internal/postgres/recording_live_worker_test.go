package postgres

import (
	"context"
	"errors"
	"testing"

	"hhc/asset-api/internal/migrations"
)

func TestLiveWorkerSingleOwnerReleasesOnFailureAndCancellation(t *testing.T) {
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingCaptureStore(db)
	sentinel := errors.New("worker stopped")
	for _, cancelOwner := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		acquired, err := store.WithLiveWorker(ctx, func(ctx context.Context, check func(context.Context) error) error {
			if err := check(ctx); err != nil {
				t.Fatal(err)
			}
			busy, e := store.WithLiveWorker(context.Background(), func(context.Context, func(context.Context) error) error { t.Fatal("second standby owner"); return nil })
			if busy || e != nil {
				t.Fatalf("contender blocked normal processing: %v %v", busy, e)
			}
			var slots int
			if e := db.QueryRow(`SELECT count(*) FROM recording_processing_slots WHERE leased_until>now()`).Scan(&slots); e != nil || slots != 0 {
				t.Fatalf("standby consumed processing capacity: %d %v", slots, e)
			}
			if cancelOwner {
				cancel()
				return ctx.Err()
			}
			return sentinel
		})
		cancel()
		if !acquired || (!cancelOwner && !errors.Is(err, sentinel)) || (cancelOwner && !errors.Is(err, context.Canceled)) {
			t.Fatalf("owner result: %v %v", acquired, err)
		}
		acquired, err = store.WithLiveWorker(context.Background(), func(ctx context.Context, check func(context.Context) error) error { return check(ctx) })
		if !acquired || err != nil {
			t.Fatalf("lock leaked: %v %v", acquired, err)
		}
	}
}

func TestLiveWorkerStopsAdmissionAfterLockConnectionLoss(t *testing.T) {
	db := isolatedIntegrationDB(t)
	db.SetMaxOpenConns(2)
	var ownerPID int
	if err := db.QueryRow(`SELECT pg_backend_pid()`).Scan(&ownerPID); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingCaptureStore(db)
	owned, err := store.WithLiveWorker(context.Background(), func(ctx context.Context, check func(context.Context) error) error {
		// The pool has exactly one idle session before election: terminate only it.
		var killed bool
		if err := db.QueryRow(`SELECT pg_terminate_backend($1,5000)`, ownerPID).Scan(&killed); err != nil || !killed {
			t.Fatalf("terminate owner: %v", err)
		}
		if err := check(ctx); err == nil {
			t.Fatal("lost owner admitted another claim")
		}
		return nil
	})
	if !owned || err == nil {
		t.Fatalf("lost session must report unlock failure: %v %v", owned, err)
	}
	owned, err = store.WithLiveWorker(context.Background(), func(context.Context, func(context.Context) error) error { return nil })
	if !owned || err != nil {
		t.Fatalf("new owner did not recover: %v %v", owned, err)
	}
}
