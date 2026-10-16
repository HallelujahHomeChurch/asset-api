package postgres

import (
	"context"
	"errors"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
	"testing"
)

func TestAutomaticCoverWaitsForPublishedBroadcastEndWithoutSpendingAttempts(t *testing.T) {
	for _, kind := range []string{"auto", "live-auto", "custom"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			db := isolatedIntegrationDB(t)
			if err := migrations.Run(ctx, db); err != nil {
				t.Fatal(err)
			}
			pkg := testRecordingPackage(t, "waiting-cover", "actor", "waiting-recording")
			if err := NewRecordingPackageStore(db).Create(ctx, pkg); err != nil {
				t.Fatal(err)
			}
			queries := []string{
				`UPDATE recording_packages SET state='ready',broadcast_epoch=1,completed_at=now(),ready_at=now(),final_prefix='recordings/packages/waiting-cover/final/one/',media_expires_at=now()+interval '30 days' WHERE id='waiting-cover'`,
				`INSERT INTO recording_captures(id,actor_id,recording_id,create_key,state,created_at,expires_at,package_id,broadcast_epoch) VALUES('waiting-cover','actor','waiting-recording','waiting','ready',now(),now()+interval '1 hour','waiting-cover',1)`,
				`INSERT INTO recording_broadcast_ranges(capture_id,recording_id,epoch,range_revision,start_sequence) VALUES('waiting-cover','waiting-recording',1,1,0)`,
				`INSERT INTO recording_live(capture_id,segments,published_sequence) VALUES('waiting-cover','[]',-1)`,
			}
			for _, q := range queries {
				if _, err := db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`INSERT INTO recording_covers(id,package_id,recording_id,actor_id,idempotency_key,kind,state,expires_at) VALUES('cover','waiting-cover','waiting-recording','actor','cover',$1,'pending',now()+interval '30 days')`, kind); err != nil {
				t.Fatal(err)
			}
			store := NewRecordingCoverStore(db)
			if kind == "custom" {
				if c, err := store.Claim(ctx); err != nil || c.ID != "cover" {
					t.Fatalf("custom cover blocked by open range: %v %v", c, err)
				}
				return
			}
			assertWaiting := func() {
				t.Helper()
				if _, err := store.Claim(ctx); !errors.Is(err, assets.ErrNotFound) {
					t.Fatalf("claimed unavailable timeline: %v", err)
				}
				var attempts int
				if err := db.QueryRow(`SELECT attempts FROM recording_covers WHERE id='cover'`).Scan(&attempts); err != nil || attempts != 0 {
					t.Fatalf("attempts=%d err=%v", attempts, err)
				}
			}
			assertWaiting()
			if _, err := db.Exec(`UPDATE recording_broadcast_ranges SET end_sequence_exclusive=1`); err != nil {
				t.Fatal(err)
			}
			assertWaiting()
			if _, err := db.Exec(`UPDATE recording_live SET segments='[{"sequence":0}]',published_sequence=0`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE recording_broadcast_ranges SET revoked=true`); err != nil {
				t.Fatal(err)
			}
			assertWaiting()
			if _, err := db.Exec(`UPDATE recording_broadcast_ranges SET revoked=false`); err != nil {
				t.Fatal(err)
			}
			if c, err := store.Claim(ctx); err != nil || c.ID != "cover" {
				t.Fatalf("ready boundary not claimed: %v %v", c, err)
			}
		})
	}
}
