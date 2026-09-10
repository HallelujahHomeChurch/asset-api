package postgres

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func insertPersonalQuotaAsset(t *testing.T, db *sql.DB, id, owner string, size int64, now time.Time) {
	t.Helper()
	insertAsset(t, db, id, assets.UploadCompleted, assets.ScanClean, assets.ProcessingNotRequired, now, time.Time{})
	if _, err := db.Exec(`UPDATE assets SET namespace='presenter.personal',owner_service='presenter.personal',owner_type='user',owner_id=$2,size_bytes=$3 WHERE id=$1`, id, owner, size); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO upload_sessions(id,asset_id,idempotency_key,caller_service,operation,request_fingerprint,staging_object_key,max_size_bytes,status,expires_at,created_at) VALUES($1,$1,$1,'presenter.personal','create',$1,$1,$2,'completed',$3,$4)`, id, size, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
}

func TestPersonalUsageQuotaAndPurge(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := store.EnsurePersonalSpace(ctx, "alice", now); err != nil {
		t.Fatal(err)
	}

	insertPersonalQuotaAsset(t, db, "active", "alice", 20, now)
	active, err := store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "active", Type: "create-file", ItemID: "active-item", Name: "active.pdf", UploadID: "active"}, now)
	if err != nil {
		t.Fatal(err)
	}
	insertPersonalQuotaAsset(t, db, "trash", "alice", 20, now)
	trash, err := store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "trash", Type: "create-file", ItemID: "trash-item", Name: "trash.pdf", UploadID: "trash"}, now)
	if err != nil {
		t.Fatal(err)
	}
	trash, err = store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "delete-trash", Type: "delete", ItemID: "trash-item", ExpectedRevision: trash.NodeRevision}, now)
	if err != nil {
		t.Fatal(err)
	}
	insertPersonalQuotaAsset(t, db, "old", "alice", 10, now)
	replaced, err := store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "old", Type: "create-file", ItemID: "replace-item", Name: "replace.pdf", UploadID: "old"}, now)
	if err != nil {
		t.Fatal(err)
	}
	insertPersonalQuotaAsset(t, db, "new", "alice", 10, now)
	if _, err = store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "new", Type: "replace-content", ItemID: "replace-item", UploadID: "new", ExpectedRevision: replaced.NodeRevision}, now); err != nil {
		t.Fatal(err)
	}

	usage, err := store.PersonalUsage(ctx, "alice", now)
	if err != nil || usage.ActiveBytes != 30 || usage.TrashBytes != 20 || usage.ProtectedBytes != 10 || usage.UsedBytes != 60 || usage.QuotaBytes != assets.DefaultPersonalQuotaBytes {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
	limit := int64(50)
	usage, err = store.SetPersonalQuota(ctx, "admin", "alice", &limit, "quota-request", now)
	if err != nil || usage.QuotaBytes != 50 || usage.OverrideBytes == nil || *usage.OverrideBytes != 50 {
		t.Fatalf("override=%+v err=%v", usage, err)
	}
	var auditCount int
	if err = db.QueryRow(`SELECT count(*) FROM personal_cloud_quota_audit WHERE owner_user_id='alice' AND actor_user_id='admin' AND request_id='quota-request' AND old_quota_bytes IS NULL AND new_quota_bytes=50`).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("quota audit count=%d err=%v", auditCount, err)
	}
	if _, err = store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "rename-over", Type: "rename", ItemID: "active-item", Name: "renamed.pdf", ExpectedRevision: active.NodeRevision}, now); err != nil {
		t.Fatalf("metadata mutation while over quota: %v", err)
	}
	restored, err := store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "restore-over", Type: "restore", ItemID: "trash-item", ExpectedRevision: trash.NodeRevision}, now)
	if err != nil {
		t.Fatalf("restore while over quota: %v", err)
	}
	trash, err = store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "delete-again", Type: "delete", ItemID: "trash-item", ExpectedRevision: restored.NodeRevision}, now)
	if err != nil {
		t.Fatal(err)
	}
	insertPersonalQuotaAsset(t, db, "blocked", "alice", 1, now)
	_, err = store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "blocked", Type: "create-file", ItemID: "blocked-item", Name: "blocked.pdf", UploadID: "blocked"}, now)
	var exceeded *assets.PersonalQuotaExceeded
	if !errors.As(err, &exceeded) || exceeded.UsedBytes != 60 || exceeded.QuotaBytes != 50 || exceeded.RequiredBytes != 1 {
		t.Fatalf("quota error=%+v", err)
	}

	purged, err := store.PurgePersonalTrash(ctx, "alice", assets.PersonalTrashPurgeInput{OperationID: "purge", ItemIDs: []string{"trash-item"}}, now)
	if err != nil || len(purged.PurgedItemIDs) != 1 || purged.PurgedItemIDs[0] != "trash-item" {
		t.Fatalf("purge=%+v err=%v", purged, err)
	}
	replay, err := store.PurgePersonalTrash(ctx, "alice", assets.PersonalTrashPurgeInput{OperationID: "purge", ItemIDs: []string{"trash-item"}}, now.Add(time.Hour))
	if err != nil || len(replay.PurgedItemIDs) != 1 {
		t.Fatalf("purge replay=%+v err=%v", replay, err)
	}
	usage, err = store.PersonalUsage(ctx, "alice", now)
	if err != nil || usage.UsedBytes != 40 {
		t.Fatalf("post-purge usage=%+v err=%v", usage, err)
	}
}

func TestPersonalQuotaSerializesConcurrentCommits(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := store.EnsurePersonalSpace(ctx, "alice", now); err != nil {
		t.Fatal(err)
	}
	limit := int64(10)
	if _, err := store.SetPersonalQuota(ctx, "admin", "alice", &limit, "limit", now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		insertPersonalQuotaAsset(t, db, id, "alice", 6, now)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for index, id := range []string{"first", "second"} {
		wg.Add(1)
		go func(index int, id string) {
			defer wg.Done()
			_, errs[index] = store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: id, Type: "create-file", ItemID: id + "-item", Name: id + ".pdf", UploadID: id}, now)
		}(index, id)
	}
	wg.Wait()
	var successes, rejected int
	for _, err := range errs {
		if err == nil {
			successes++
		} else if errors.Is(err, assets.ErrPersonalQuotaExceeded) {
			rejected++
		} else {
			t.Fatalf("unexpected commit error: %v", err)
		}
	}
	if successes != 1 || rejected != 1 {
		t.Fatalf("successes=%d rejected=%d errors=%v", successes, rejected, errs)
	}
}

func TestPersonalUsageCountsAnActiveAndTrashedSharedAssetOnce(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := store.EnsurePersonalSpace(ctx, "alice", now); err != nil {
		t.Fatal(err)
	}
	insertPersonalQuotaAsset(t, db, "shared", "alice", 10, now)
	created, err := store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "create-old", Type: "create-file", ItemID: "old-item", Name: "old.pdf", UploadID: "shared"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "delete-old", Type: "delete", ItemID: "old-item", ExpectedRevision: created.NodeRevision}, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "create-new", Type: "create-file", ItemID: "new-item", Name: "new.pdf", UploadID: "shared"}, now); err != nil {
		t.Fatal(err)
	}
	usage, err := store.PersonalUsage(ctx, "alice", now)
	if err != nil || usage.ActiveBytes != 10 || usage.TrashBytes != 0 || usage.UsedBytes != 10 {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
}
