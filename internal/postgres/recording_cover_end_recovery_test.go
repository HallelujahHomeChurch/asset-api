package postgres

import (
	"context"
	"hhc/asset-api/internal/migrations"
	"testing"
)

func TestEndWaitRecoveryIsExactFencedAndOneTime(t *testing.T) {
	for _, tc := range []struct {
		name, alter string
		want        int
	}{
		{"eligible", "", 2},
		{"revoked", `UPDATE recording_broadcast_ranges SET revoked=true`, 0},
		{"open", `UPDATE recording_broadcast_ranges SET end_sequence_exclusive=NULL`, 0},
		{"unpublished", `UPDATE recording_live SET published_sequence=8`, 0},
		{"expired", `UPDATE recording_packages SET media_expires_at=now()-interval '1 minute'`, 0},
		{"deleted", `INSERT INTO recording_deletions(recording_id) VALUES('89cf022d-e80e-479f-9bbe-d8590771cc59')`, 0},
		{"terminal", `UPDATE recording_captures SET terminal_at=now()`, 0},
		{"custom", `UPDATE recording_covers SET kind='custom'`, 0},
		{"already-ready", `UPDATE recording_covers SET state='ready'`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := isolatedIntegrationDB(t)
			if err := migrations.Run(ctx, db); err != nil {
				t.Fatal(err)
			}
			const capture = "32264350a74315406f3d119a72023cce"
			const recording = "89cf022d-e80e-479f-9bbe-d8590771cc59"
			pkg := testRecordingPackage(t, capture, "actor", recording)
			if err := NewRecordingPackageStore(db).Create(ctx, pkg); err != nil {
				t.Fatal(err)
			}
			if err := NewRecordingPackageStore(db).Create(ctx, testRecordingPackage(t, "unrelated-package", "actor", "unrelated-recording")); err != nil {
				t.Fatal(err)
			}
			for _, q := range []string{
				`DELETE FROM schema_migrations WHERE version='sql/042_recording_cover_end_wait_recovery.sql'`,
				`UPDATE recording_packages SET state='ready',broadcast_epoch=1,completed_at=now(),ready_at=now(),final_prefix='recordings/packages/recovery/final/one/',media_expires_at=now()+interval '30 days'`,
				`INSERT INTO recording_captures(id,actor_id,recording_id,create_key,state,created_at,expires_at,package_id,broadcast_epoch) VALUES('32264350a74315406f3d119a72023cce','actor','89cf022d-e80e-479f-9bbe-d8590771cc59','recovery','ready',now(),now()+interval '1 hour','32264350a74315406f3d119a72023cce',1)`,
				`INSERT INTO recording_broadcast_ranges(capture_id,recording_id,epoch,range_revision,start_sequence,end_sequence_exclusive) VALUES('32264350a74315406f3d119a72023cce','89cf022d-e80e-479f-9bbe-d8590771cc59',1,4,0,10)`,
				`INSERT INTO recording_live(capture_id,segments,published_sequence) SELECT '32264350a74315406f3d119a72023cce',jsonb_agg(jsonb_build_object('sequence',n)),9 FROM generate_series(0,9) n`,
				`INSERT INTO recording_covers(id,package_id,recording_id,actor_id,idempotency_key,kind,state,attempts,expires_at) SELECT id,'32264350a74315406f3d119a72023cce','89cf022d-e80e-479f-9bbe-d8590771cc59','actor',id,kind,'failed',3,now()+interval '30 days' FROM (VALUES('40d710ca35646d68c18bd54684693113','live-auto'),('ce9d53c03737e84af2b8369acba2a434','auto')) v(id,kind)`,
				`INSERT INTO recording_covers(id,package_id,recording_id,actor_id,idempotency_key,kind,state,attempts,expires_at) VALUES('unrelated','unrelated-package','unrelated-recording','actor','unrelated','auto','failed',3,now()+interval '30 days')`,
				`INSERT INTO recording_cover_attempts(claim_id,cover_id) SELECT id,id FROM recording_covers`,
			} {
				if _, err := db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			if tc.alter != "" {
				if _, err := db.Exec(tc.alter); err != nil {
					t.Fatal(err)
				}
			}
			if err := migrations.Run(ctx, db); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM recording_covers WHERE state='pending' AND attempts=0`).Scan(&count); err != nil || count != tc.want {
				t.Fatalf("recovered %d want %d: %v", count, tc.want, err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM recording_cover_attempts`).Scan(&count); err != nil || count != 3 {
				t.Fatal("attempt history changed", count, err)
			}
			if _, err := db.Exec(`UPDATE recording_covers SET state='failed',attempts=3`); err != nil {
				t.Fatal(err)
			}
			if err := migrations.Run(ctx, db); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM recording_covers WHERE state='pending'`).Scan(&count); err != nil || count != 0 {
				t.Fatal("recovery repeated", count, err)
			}
		})
	}
}
