package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
)

func TestRecordingCleanupDoesNotStarveLaterItems(t *testing.T) {
	for _, kind := range []string{"source", "package"} {
		for _, canceled := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/failure", true: "/cancellation"}[canceled], func(t *testing.T) {
				ctx := context.Background()
				db := isolatedIntegrationDB(t)
				if err := migrations.Run(ctx, db); err != nil {
					t.Fatal(err)
				}
				table := "recording_packages"
				ids := []string{"first", "second"}
				packages := NewRecordingPackageStore(db)
				sources := NewRecordingSourceStore(db)
				for i, id := range ids {
					if kind == "package" {
						if err := packages.Create(ctx, testRecordingPackage(t, id, id, id)); err != nil {
							t.Fatal(err)
						}
					} else {
						table = "recording_sources"
						svc := assets.NewRecordingSourceService(sources, &sourceBlobFixture{}, time.Now)
						p, err := svc.Create(ctx, assets.CreateRecordingSourceInput{ActorID: id, RecordingID: id, FileName: "source.mp4", SizeBytes: 5, ChecksumSHA256: strings.Repeat("a", 64), IdempotencyKey: id})
						if err != nil {
							t.Fatal(err)
						}
						ids[i] = p.ID
					}
					if _, err := db.ExecContext(ctx, "UPDATE "+table+" SET state='expired', cleanup_after=clock_timestamp()-($2 * interval '1 hour') WHERE id=$1", ids[i], 2-i); err != nil {
						t.Fatal(err)
					}
				}
				run := func(ctx context.Context, remove func(context.Context, string) error) error {
					if kind == "source" {
						return sources.ReconcileSources(ctx, func(ctx context.Context, id string, _ []string) error { return remove(ctx, id) }, func(context.Context, string) error { t.Fatal("unexpected attempt deletion"); return nil })
					}
					return packages.ReconcilePackages(ctx, func(ctx context.Context, keys []string) error { return remove(ctx, strings.Split(keys[0], "/")[2]) })
				}
				failure := errors.New("provider failure")
				attemptCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				var deleted []string
				err := run(attemptCtx, func(_ context.Context, id string) error {
					if id == ids[0] {
						if canceled {
							cancel()
							return context.Canceled
						}
						return failure
					}
					deleted = append(deleted, id)
					return nil
				})
				want := failure
				if canceled {
					want = context.Canceled
				}
				if !errors.Is(err, want) {
					t.Fatalf("lost failure: %v", err)
				}
				if !canceled && (len(deleted) != 1 || deleted[0] != ids[1]) {
					t.Fatalf("later item starved: %v", deleted)
				}
				var deferred bool
				if err := db.QueryRowContext(ctx, "SELECT cleanup_after>clock_timestamp() AND cleanup_after<clock_timestamp()+interval '10 minutes' FROM "+table+" WHERE id=$1", ids[0]).Scan(&deferred); err != nil || !deferred {
					t.Fatalf("failed item not deferred: %v %v", deferred, err)
				}
				if canceled {
					if err := run(ctx, func(_ context.Context, id string) error { deleted = append(deleted, id); return nil }); err != nil {
						t.Fatal(err)
					}
					if len(deleted) != 1 || deleted[0] != ids[1] {
						t.Fatalf("unattempted item did not proceed next run: %v", deleted)
					}
				}
				if _, err := db.ExecContext(ctx, "UPDATE "+table+" SET cleanup_after=clock_timestamp()-interval '1 second' WHERE id=$1", ids[0]); err != nil {
					t.Fatal(err)
				}
				deleted = nil
				if err := run(ctx, func(_ context.Context, id string) error { deleted = append(deleted, id); return nil }); err != nil {
					t.Fatal(err)
				}
				if len(deleted) != 1 || deleted[0] != ids[0] {
					t.Fatalf("failed item cannot retry: %v", deleted)
				}
			})
		}
	}
}

func TestPackageCleanupSkipsTenProtectedCaptures(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingCaptureStore(db)
	packages := NewRecordingPackageStore(db)
	var protected []string
	for i := 1; i <= 10; i++ {
		id := sealedLiveCapture(t, store, i)
		protected = append(protected, id)
		if _, err := db.ExecContext(ctx, `UPDATE recording_packages SET state='ready',final_prefix='recordings/packages/'||id||'/final/test/',ready_at=now(),media_expires_at=now()+interval '30 days',cleanup_after=now()-($2*interval '1 minute') WHERE id=$1`, id, 12-i); err != nil {
			t.Fatal(err)
		}
	}
	p := testRecordingPackage(t, "unprotected", "other-actor", "other-recording")
	if err := packages.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_packages SET state='failed',cleanup_after=now()-interval '1 minute' WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 2; run++ {
		var deleted []string
		if err := packages.ReconcilePackages(ctx, func(_ context.Context, keys []string) error { deleted = append(deleted, keys...); return nil }); err != nil {
			t.Fatal(err)
		}
		if len(deleted) != len(p.Inventory.Objects) {
			t.Fatalf("11th due package starved on run %d: %v", run, deleted)
		}
		for _, key := range deleted {
			if !strings.Contains(key, "/unprotected/") {
				t.Fatalf("protected capture removed: %s", key)
			}
		}
		if _, err := db.ExecContext(ctx, `UPDATE recording_packages SET cleanup_after=now()-interval '1 second' WHERE id=$1`, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Eligibility, rather than a blanket reschedule, releases every previously
	// protected candidate after its original capture deadline plus writer grace.
	for _, id := range protected {
		if _, err := db.ExecContext(ctx, `UPDATE recording_captures SET created_at=now()-interval '2 days',expires_at=now()-interval '1 day' WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	var released []string
	if err := packages.ReconcilePackages(ctx, func(_ context.Context, keys []string) error { released = append(released, keys...); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(released) != 100 {
		t.Fatalf("eligible capture packages stayed protected: %v", released)
	}
}
