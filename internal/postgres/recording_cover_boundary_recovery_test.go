package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
	"math"
	"testing"
)

func TestCoverBoundaryRecoveryMigrationFencesAndRunsOnce(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if e := migrations.Run(ctx, db); e != nil {
		t.Fatal(e)
	}
	const migration = "sql/041_recording_cover_boundary_recovery.sql"
	if _, e := db.Exec(`DELETE FROM schema_migrations WHERE version=$1`, migration); e != nil {
		t.Fatal(e)
	}
	wanted := map[string]string{}
	for i, tc := range []struct {
		name, kind, state                      string
		revoked, expired, deleted, offBoundary bool
		want                                   string
	}{
		{name: "auto", kind: "auto", state: "failed", want: "pending"}, {name: "inherited", kind: "live-auto", state: "failed", want: "pending"},
		{name: "custom", kind: "custom", state: "failed", want: "failed"}, {name: "ready", kind: "auto", state: "ready", want: "ready"},
		{name: "revoked", kind: "auto", state: "failed", revoked: true, want: "failed"}, {name: "expired", kind: "auto", state: "failed", expired: true, want: "failed"},
		{name: "deleted", kind: "auto", state: "failed", deleted: true, want: "failed"}, {name: "off-boundary", kind: "auto", state: "failed", offBoundary: true, want: "failed"},
	} {
		id, recording := fmt.Sprintf("recovery-capture-%d", i), fmt.Sprintf("recovery-recording-%d", i)
		pkg := testRecordingPackage(t, id, "actor", recording)
		if e := NewRecordingPackageStore(db).Create(ctx, pkg); e != nil {
			t.Fatal(e)
		}
		if _, e := db.Exec(`UPDATE recording_packages SET state='ready',ready_at=now(),media_expires_at=now()+interval '30 days' WHERE id=$1`, id); e != nil {
			t.Fatal(e)
		}
		if _, e := db.Exec(`INSERT INTO recording_captures(id,actor_id,recording_id,create_key,state,created_at,expires_at,package_id) VALUES($1,'actor',$2,$1,'ready',now(),now()+interval '1 hour',$1)`, id, recording); e != nil {
			t.Fatal(e)
		}
		timeline := make([]assets.RecordingLiveSegment, 14)
		for s := range timeline {
			timeline[s] = assets.RecordingLiveSegment{Sequence: s, Renditions: map[string]assets.RecordingLiveFragment{"480p": {Start: math.Round((.021+float64(s)*30.03)*1000) / 1000, End: math.Round((.021+float64(s+1)*30.03)*1000) / 1000}}}
		}
		raw, _ := json.Marshal(timeline)
		end := 14
		if tc.offBoundary {
			end = 7
		}
		if _, e := db.Exec(`INSERT INTO recording_live(capture_id,segments) VALUES($1,$2)`, id, raw); e != nil {
			t.Fatal(e)
		}
		if _, e := db.Exec(`INSERT INTO recording_broadcast_ranges(capture_id,recording_id,epoch,range_revision,start_sequence,end_sequence_exclusive,revoked) VALUES($1,$2,1,1,0,$3,$4)`, id, recording, end, tc.revoked); e != nil {
			t.Fatal(e)
		}
		if _, e := db.Exec(`INSERT INTO recording_covers(id,package_id,recording_id,actor_id,idempotency_key,kind,state,attempts,expires_at) VALUES($1,$2,$3,'test',$1,$4,$5,3,now()+interval '30 days')`, tc.name, id, recording, tc.kind, tc.state); e != nil {
			t.Fatal(e)
		}
		if _, e := db.Exec(`INSERT INTO recording_cover_attempts(claim_id,cover_id) VALUES($1,$1)`, tc.name); e != nil {
			t.Fatal(e)
		}
		if tc.expired {
			if _, e := db.Exec(`UPDATE recording_packages SET media_expires_at=now()-interval '1 minute' WHERE id=$1`, id); e != nil {
				t.Fatal(e)
			}
		}
		if tc.deleted {
			if _, e := db.Exec(`INSERT INTO recording_deletions(recording_id) VALUES($1)`, recording); e != nil {
				t.Fatal(e)
			}
		}
		wanted[tc.name] = tc.want
	}
	if e := migrations.Run(ctx, db); e != nil {
		t.Fatal(e)
	}
	rows, e := db.Query(`SELECT id,state,attempts FROM recording_covers ORDER BY id`)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	for rows.Next() {
		var id, state string
		var attempts int
		if e := rows.Scan(&id, &state, &attempts); e != nil {
			t.Fatal(e)
		}
		want := wanted[id]
		if id == "auto" || id == "inherited" {
			want = "pending"
			if attempts != 0 {
				t.Fatal("retry budget not renewed")
			}
		}
		if state != want {
			t.Fatalf("%s got %s want %s", id, state, want)
		}
	}
	if e := rows.Err(); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(`UPDATE recording_covers SET state='failed',attempts=3 WHERE id='auto'`); e != nil {
		t.Fatal(e)
	}
	if e = migrations.Run(ctx, db); e != nil {
		t.Fatal(e)
	}
	var state string
	if e = db.QueryRow(`SELECT state FROM recording_covers WHERE id='auto'`).Scan(&state); e != nil || state != "failed" {
		t.Fatal("migration repeated recovery", e)
	}
	var count int
	if e = db.QueryRow(`SELECT count(*) FROM recording_cover_attempts`).Scan(&count); e != nil || count != 8 {
		t.Fatal("attempt history changed", count, e)
	}
}
