package postgres

import (
	"context"
	"errors"
	"hhc/asset-api/internal/assets"
	"testing"
	"time"
)

func TestPersonalRetentionAndExpiredRestore(t *testing.T) {
	for _, test := range []struct {
		name      string
		trashAge  time.Duration
		wantPurge bool
	}{
		{"active", 0, false}, {"trash-29-days", 29 * 24 * time.Hour, false}, {"trash-31-days", 31 * 24 * time.Hour, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := integrationDB(t)
			store := New(db)
			ctx := context.Background()
			now := time.Now().UTC()
			created := now.Add(-40 * 24 * time.Hour)
			if _, err := store.EnsurePersonalSpace(ctx, "alice", created); err != nil {
				t.Fatal(err)
			}
			insertAsset(t, db, "asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingNotRequired, created, time.Time{})
			if _, err := db.Exec(`UPDATE assets SET namespace='presenter.personal',owner_service='presenter.personal',owner_type='user',owner_id='alice' WHERE id='asset'`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO upload_sessions(id,asset_id,idempotency_key,caller_service,operation,request_fingerprint,staging_object_key,max_size_bytes,status,expires_at,created_at) VALUES('upload','asset','upload','presenter.personal','create','upload','staging',100,'completed',$1,$1)`, created); err != nil {
				t.Fatal(err)
			}
			head, err := store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "file", Type: "create-file", ItemID: "file", Name: "file.pdf", UploadID: "upload"}, created)
			if err != nil {
				t.Fatal(err)
			}
			if test.trashAge > 0 {
				deleted, err := store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "delete", Type: "delete", ItemID: "file", ExpectedRevision: head.NodeRevision}, now.Add(-test.trashAge))
				if err != nil {
					t.Fatal(err)
				}
				if test.wantPurge {
					if _, err = store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "expired-restore", Type: "restore", ItemID: "file", ExpectedRevision: deleted.NodeRevision}, now); !errors.Is(err, assets.ErrNotFound) {
						t.Fatalf("expired restore=%v", err)
					}
				}
			}
			candidate, found, err := store.ClaimPurge(ctx, now, time.Minute)
			if err != nil || found != test.wantPurge || (found && candidate.AssetID != "asset") {
				t.Fatalf("purge=%+v found=%v err=%v", candidate, found, err)
			}
		})
	}
}

func TestPersonalOrphanStagingWaitsOneDayAndActiveUpload(t *testing.T) {
	for _, test := range []struct {
		name         string
		age          time.Duration
		active, want bool
	}{
		{"young", 23 * time.Hour, false, false}, {"expired", 25 * time.Hour, false, true}, {"active", 25 * time.Hour, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := integrationDB(t)
			store := New(db)
			now := time.Now().UTC()
			created := now.Add(-test.age)
			expires := created.Add(10 * time.Minute)
			if test.active {
				expires = now.Add(time.Hour)
			}
			insertAsset(t, db, "orphan", assets.UploadCreated, assets.ScanPending, assets.ProcessingNotRequired, created, time.Time{})
			if _, err := db.Exec(`UPDATE assets SET namespace='presenter.personal',owner_service='presenter.personal',owner_type='user',owner_id='alice' WHERE id='orphan'`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO upload_sessions(id,asset_id,idempotency_key,caller_service,operation,request_fingerprint,staging_object_key,max_size_bytes,status,expires_at,created_at) VALUES('upload','orphan','upload','presenter.personal','create','upload','staging',100,'created',$1,$2)`, expires, created); err != nil {
				t.Fatal(err)
			}
			_, found, err := store.ClaimPurge(context.Background(), now, time.Minute)
			if err != nil || found != test.want {
				t.Fatalf("found=%v err=%v", found, err)
			}
		})
	}
}
