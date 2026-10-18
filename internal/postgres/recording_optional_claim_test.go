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

func TestOptionalMediaUsesFreeSlotBesideClaimedValidation(t *testing.T) {
	for _, kind := range []string{"live", "package", "source"} {
		for _, optional := range []string{"cover", "preview"} {
			t.Run(kind+"/"+optional, func(t *testing.T) {
				ctx := context.Background()
				db := isolatedIntegrationDB(t)
				if err := migrations.Run(ctx, db); err != nil {
					t.Fatal(err)
				}
				exec := func(q string, args ...any) {
					t.Helper()
					if _, err := db.ExecContext(ctx, q, args...); err != nil {
						t.Fatal(err)
					}
				}
				cap := assets.RecordingCapture{ID: strings.Repeat("e", 32), ActorID: "actor", RecordingID: "recording", CreateKey: "optional", State: "uploading", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}
				if _, err := NewRecordingCaptureStore(db).CreateCapture(ctx, cap); err != nil {
					t.Fatal(err)
				}
				exec(`UPDATE recording_live SET segments='[{"sequence":0,"renditions":{"480p":{"start":0,"end":10.01}}}]',revision=1,published_revision=1,published_sequence=0 WHERE capture_id=$1`, cap.ID)
				packages := NewRecordingPackageStore(db)
				covers := NewRecordingLiveCoverStore(db)
				if optional == "cover" {
					c, err := covers.Create(ctx, cap.ID, cap.RecordingID, "actor", "auto", "image/jpeg", "", "auto")
					if err != nil {
						t.Fatal(err)
					}
					if err = covers.Queue(ctx, c.ID); err != nil {
						t.Fatal(err)
					}
				}
				var claim func() error
				switch kind {
				case "live":
					exec(`UPDATE recording_live SET revision=2 WHERE capture_id=$1`, cap.ID)
					claim = func() error { _, err := NewRecordingCaptureStore(db).ClaimLiveValidation(ctx); return err }
				case "package":
					p := testRecordingPackage(t, "package", "package-actor", "package-recording")
					if err := packages.Create(ctx, p); err != nil {
						t.Fatal(err)
					}
					if err := packages.Freeze(ctx, p.ID, time.Now()); err != nil {
						t.Fatal(err)
					}
					claim = func() error { _, err := packages.ClaimPackageValidation(ctx); return err }
				case "source":
					store := NewRecordingSourceStore(db)
					svc := assets.NewRecordingSourceService(store, &sourceBlobFixture{}, time.Now)
					p, err := svc.Create(ctx, assets.CreateRecordingSourceInput{ActorID: "source-actor", RecordingID: "source-recording", FileName: "source.mp4", SizeBytes: 5, ChecksumSHA256: strings.Repeat("a", 64), IdempotencyKey: "source"})
					if err != nil {
						t.Fatal(err)
					}
					if _, err = svc.Complete(ctx, p.ID, p.ActorID); err != nil {
						t.Fatal(err)
					}
					claim = func() error { _, err := store.ClaimSourceProcessing(ctx); return err }
				}
				claimOptional := func() error {
					if optional == "cover" {
						_, err := covers.Claim(ctx)
						return err
					}
					_, err := packages.ClaimLivePreview(ctx)
					return err
				}
				if err := claimOptional(); !errors.Is(err, assets.ErrNotFound) {
					t.Fatalf("waiting validation must have priority: %v", err)
				}
				if err := claim(); err != nil {
					t.Fatal(err)
				}
				if err := claimOptional(); err != nil {
					t.Fatalf("claimed validation starved free second slot: %v", err)
				}
				var slots int
				if err := db.QueryRowContext(ctx, `SELECT count(*) FROM recording_processing_slots WHERE leased_until>now()`).Scan(&slots); err != nil || slots != 2 {
					t.Fatalf("slots=%d err=%v", slots, err)
				}
				if err := claimOptional(); !errors.Is(err, assets.ErrNotFound) {
					t.Fatalf("exceeded global slots: %v", err)
				}
			})
		}
	}
}
