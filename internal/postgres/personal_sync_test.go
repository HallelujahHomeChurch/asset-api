package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestPersonalAtomicDirectory(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Now().UTC()
	var roots [2]assets.PersonalSpace
	var errs [2]error
	var wg sync.WaitGroup
	for i := range roots {
		wg.Add(1)
		go func(i int) { defer wg.Done(); roots[i], errs[i] = store.EnsurePersonalSpace(ctx, "alice", now) }(i)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || roots[0].ID != roots[1].ID {
		t.Fatalf("roots=%+v errors=%v", roots, errs)
	}
	create := assets.PersonalMutation{OperationID: "create", Type: "create-folder", ItemID: "parent", Name: "Folder"}
	first, err := store.ApplyPersonalMutation(ctx, "alice", create, now)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.ApplyPersonalMutation(ctx, "alice", create, now.Add(time.Hour))
	if err != nil || replay != first {
		t.Fatalf("retry=%+v err=%v", replay, err)
	}
	create.Name = "different"
	if _, err = store.ApplyPersonalMutation(ctx, "alice", create, now); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("changed receipt: %v", err)
	}
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: []string{"rename-a", "rename-b"}[i], Type: "rename", ItemID: "parent", ExpectedRevision: first.NodeRevision, Name: []string{"A", "B"}[i]}, now)
		}(i)
	}
	wg.Wait()
	successes, conflicts := 0, 0
	for _, err := range errs {
		if err == nil {
			successes++
		} else if errors.Is(err, assets.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflict=%d", successes, conflicts)
	}
	if _, err = store.EnsurePersonalSpace(ctx, "bob", now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ApplyPersonalMutation(ctx, "bob", assets.PersonalMutation{OperationID: "cross", Type: "create-folder", ItemID: "other", ParentID: "parent", Name: "Other"}, now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("cross-owner parent: %v", err)
	}
	child, err := store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "child", Type: "create-folder", ItemID: "child", ParentID: "parent", Name: "Child"}, now)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.GetPersonalNode(ctx, "alice", "parent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "cycle", Type: "move", ItemID: "parent", ParentID: "child", ExpectedRevision: parent.Revision}, now); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("cycle: %v", err)
	}
	if _, err = store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "delete-stale", Type: "delete", ItemID: "parent", ExpectedRevision: parent.Revision, ExpectedCollectionRevision: first.CollectionRevision}, now); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("stale subtree: %v", err)
	}
	deleted, err := store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "delete", Type: "delete", ItemID: "parent", ExpectedRevision: parent.Revision, ExpectedCollectionRevision: child.CollectionRevision}, now)
	if err != nil {
		t.Fatal(err)
	}
	node, err := store.GetPersonalNode(ctx, "alice", "child")
	if err != nil || node.DeletedAt == nil {
		t.Fatalf("child deletion=%+v %v", node, err)
	}
	_, err = store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "restore", Type: "restore", ItemID: "parent", ExpectedRevision: deleted.NodeRevision, ExpectedCollectionRevision: deleted.CollectionRevision}, now)
	if err != nil {
		t.Fatal(err)
	}
	node, err = store.GetPersonalNode(ctx, "alice", "child")
	if err != nil || node.DeletedAt != nil {
		t.Fatalf("child restoration=%+v %v", node, err)
	}
	// Force an error after directory writes; the head, event and receipt must roll back together.
	if _, err = db.Exec(`CREATE FUNCTION reject_personal_receipt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure'; END $$; CREATE TRIGGER reject_personal_receipt BEFORE INSERT ON personal_sync_receipts FOR EACH ROW EXECUTE FUNCTION reject_personal_receipt()`); err != nil {
		t.Fatal(err)
	}
	before, err := store.GetPersonalNode(ctx, "alice", "parent")
	if err != nil {
		t.Fatal(err)
	}
	var beforeRevision int64
	var beforeChanges int
	if err = db.QueryRow(`SELECT revision,(SELECT count(*) FROM personal_sync_changes WHERE collection_id=$1) FROM asset_collections WHERE id=$1`, roots[0].ID).Scan(&beforeRevision, &beforeChanges); err != nil {
		t.Fatal(err)
	}
	_, err = store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "rollback", Type: "rename", ItemID: "parent", Name: "Must roll back", ExpectedRevision: before.Revision}, now)
	if err == nil {
		t.Fatal("injected transaction unexpectedly committed")
	}
	after, _ := store.GetPersonalNode(ctx, "alice", "parent")
	if after.Name != before.Name || after.Revision != before.Revision {
		t.Fatalf("partial head: %+v", after)
	}
	var afterRevision int64
	var afterChanges int
	if err = db.QueryRow(`SELECT revision,(SELECT count(*) FROM personal_sync_changes WHERE collection_id=$1) FROM asset_collections WHERE id=$1`, roots[0].ID).Scan(&afterRevision, &afterChanges); err != nil {
		t.Fatal(err)
	}
	if afterRevision != beforeRevision || afterChanges != beforeChanges {
		t.Fatalf("partial history: revision=%d changes=%d", afterRevision, afterChanges)
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM personal_sync_receipts WHERE operation_id='rollback'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial receipt %d %v", count, err)
	}
}

func TestPersonalFilesAndNameIsolation(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := store.EnsurePersonalSpace(ctx, "alice", now); err != nil {
		t.Fatal(err)
	}
	makeUpload := func(id, owner string, scan assets.ScanStatus) {
		insertAsset(t, db, id, assets.UploadCompleted, scan, assets.ProcessingNotRequired, now, time.Time{})
		if _, err := db.Exec(`UPDATE assets SET namespace='presenter.personal',owner_service='presenter.personal',owner_type='user',owner_id=$2,visibility='private' WHERE id=$1`, id, owner); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO upload_sessions(id,asset_id,idempotency_key,caller_service,operation,request_fingerprint,staging_object_key,max_size_bytes,status,expires_at,created_at) VALUES($1,$1,$1,'presenter.personal','create',$1,$1,100,'completed',$2,$3)`, id, now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
	}
	makeUpload("foreign", "bob", assets.ScanClean)
	makeUpload("pending", "alice", assets.ScanPending)
	m := assets.PersonalMutation{OperationID: "file", Type: "create-file", ItemID: "file", Name: "deck.lpdeck", UploadID: "foreign"}
	if _, err := store.ApplyPersonalMutation(ctx, "alice", m, now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("foreign asset: %v", err)
	}
	m.UploadID = "pending"
	if _, err := store.ApplyPersonalMutation(ctx, "alice", m, now); !errors.Is(err, assets.ErrPersonalAssetNotReady) {
		t.Fatalf("scan pending: %v", err)
	}
	if _, err := store.GetPersonalNode(ctx, "alice", "file"); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("pending head exposed: %v", err)
	}
	if _, err := db.Exec(`UPDATE assets SET scan_status='clean' WHERE id='pending'`); err != nil {
		t.Fatal(err)
	}
	first, err := store.ApplyPersonalMutation(ctx, "alice", m, now)
	if err != nil {
		t.Fatal(err)
	}
	makeUpload("replacement", "alice", assets.ScanPending)
	replace := assets.PersonalMutation{OperationID: "replace", Type: "replace-content", ItemID: "file", ExpectedRevision: first.NodeRevision, UploadID: "replacement"}
	if _, err = store.ApplyPersonalMutation(ctx, "alice", replace, now); !errors.Is(err, assets.ErrPersonalAssetNotReady) {
		t.Fatalf("replacement scan: %v", err)
	}
	head, _ := store.GetPersonalNode(ctx, "alice", "file")
	if head.AssetID != "pending" || head.Revision != first.NodeRevision {
		t.Fatalf("old head lost: %+v", head)
	}
	if _, err = db.Exec(`UPDATE assets SET scan_status='clean' WHERE id='replacement'`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ApplyPersonalMutation(ctx, "alice", replace, now); err != nil {
		t.Fatal(err)
	}
	head, _ = store.GetPersonalNode(ctx, "alice", "file")
	if head.AssetID != "replacement" {
		t.Fatalf("new head missing: %+v", head)
	}
	oldID, err := store.PersonalContentAssetID(ctx, "alice", "file", first.NodeRevision, now)
	if err != nil || oldID != "pending" {
		t.Fatalf("old revision=%s %v", oldID, err)
	}
	if _, err = store.PersonalContentAssetID(ctx, "bob", "file", first.NodeRevision, now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("foreign download=%v", err)
	}
	if _, err = db.Exec(`UPDATE assets SET deleted_at=$1,created_at=$1::timestamptz-interval '25 hours' WHERE id='pending'`, now); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.ClaimPurge(ctx, now.Add(time.Minute), time.Minute); err != nil || found {
		t.Fatalf("leased purge=%v %v", found, err)
	}
	if _, found, err := store.ClaimPurge(ctx, now.Add(11*time.Minute), time.Minute); err != nil || !found {
		t.Fatalf("expired lease purge=%v %v", found, err)
	}
	if _, err = store.GetPersonalNode(ctx, "bob", "file"); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("owner read: %v", err)
	}
	for i, name := range []string{"Caf\u00e9", "Cafe\u0301"} {
		_, err = store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: name, Type: "create-folder", ItemID: name, Name: name}, now)
		if i == 0 && err != nil {
			t.Fatal(err)
		}
		if i == 1 && !errors.Is(err, assets.ErrConflict) {
			t.Fatalf("NFC duplicate: %v", err)
		}
	}
}

func TestPersonalChangesSnapshotAndTransactionPagination(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Now().UTC()
	space, err := store.EnsurePersonalSpace(ctx, "alice", now)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "parent", Type: "create-folder", ItemID: "a", Name: "Parent"}, now)
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "child", Type: "create-folder", ItemID: "b", ParentID: "a", Name: "Child"}, now)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.PersonalChanges(ctx, "alice", "", 1)
	if err != nil || !first.Reset || !first.HasMore || len(first.Items) != 1 || first.Items[0].ID != "a" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	deleted, err := store.ApplyPersonalMutation(ctx, "alice", assets.PersonalMutation{OperationID: "delete", Type: "delete", ItemID: "a", ExpectedRevision: parent.NodeRevision, ExpectedCollectionRevision: child.CollectionRevision}, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.PersonalChanges(ctx, "alice", first.Cursor, 1)
	if err != nil || !second.Reset || second.HasMore || len(second.Items) != 1 || second.Items[0].ID != "b" || second.Items[0].DeletedAt != nil {
		t.Fatalf("snapshot changed=%+v err=%v", second, err)
	}
	delta1, err := store.PersonalChanges(ctx, "alice", second.Cursor, 1)
	if err != nil || delta1.Reset || !delta1.HasMore || len(delta1.Items) != 1 || delta1.Items[0].Revision != deleted.NodeRevision || delta1.Items[0].DeletedAt == nil {
		t.Fatalf("delta1=%+v err=%v", delta1, err)
	}
	delta2, err := store.PersonalChanges(ctx, "alice", delta1.Cursor, 1)
	if err != nil || delta2.HasMore || len(delta2.Items) != 1 || delta2.Items[0].ID != "b" || delta2.Items[0].DeletedAt == nil {
		t.Fatalf("delta2=%+v err=%v", delta2, err)
	}
	empty, err := store.PersonalChanges(ctx, "alice", delta2.Cursor, 1)
	if err != nil || empty.Reset || empty.HasMore || len(empty.Items) != 0 {
		t.Fatalf("empty=%+v err=%v", empty, err)
	}
	if _, err = store.EnsurePersonalSpace(ctx, "bob", now); err != nil {
		t.Fatal(err)
	}
	foreign, err := store.PersonalChanges(ctx, "bob", first.Cursor, 1)
	if err != nil || !foreign.Reset || len(foreign.Items) != 0 || foreign.Collection.ID == space.ID {
		t.Fatalf("foreign=%+v err=%v", foreign, err)
	}
	malformed, err := store.PersonalChanges(ctx, "alice", "invalid", 100)
	if err != nil || !malformed.Reset || len(malformed.Items) != 2 {
		t.Fatalf("reset=%+v err=%v", malformed, err)
	}
}

func TestPersonalRestorePreservesEarlierTrashAndFallsBackToRoot(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := store.EnsurePersonalSpace(ctx, "alice", now); err != nil {
		t.Fatal(err)
	}
	apply := func(m assets.PersonalMutation) assets.PersonalMutationResult {
		t.Helper()
		result, err := store.ApplyPersonalMutation(ctx, "alice", m, now)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	parent := apply(assets.PersonalMutation{OperationID: "parent", Type: "create-folder", ItemID: "parent", Name: "Parent"})
	child := apply(assets.PersonalMutation{OperationID: "child", Type: "create-folder", ItemID: "child", ParentID: "parent", Name: "Child"})
	oldTrash := apply(assets.PersonalMutation{OperationID: "trash-child", Type: "delete", ItemID: "child", ExpectedRevision: child.NodeRevision, ExpectedCollectionRevision: child.CollectionRevision})
	deleted := apply(assets.PersonalMutation{OperationID: "trash-parent", Type: "delete", ItemID: "parent", ExpectedRevision: parent.NodeRevision, ExpectedCollectionRevision: oldTrash.CollectionRevision})
	restored := apply(assets.PersonalMutation{OperationID: "restore-parent", Type: "restore", ItemID: "parent", ExpectedRevision: deleted.NodeRevision, ExpectedCollectionRevision: deleted.CollectionRevision})
	node, err := store.GetPersonalNode(ctx, "alice", "child")
	if err != nil || node.DeletedAt == nil || node.Revision != oldTrash.NodeRevision {
		t.Fatalf("earlier trash restored=%+v err=%v", node, err)
	}
	deleted = apply(assets.PersonalMutation{OperationID: "trash-parent-again", Type: "delete", ItemID: "parent", ExpectedRevision: restored.NodeRevision, ExpectedCollectionRevision: restored.CollectionRevision})
	apply(assets.PersonalMutation{OperationID: "restore-child", Type: "restore", ItemID: "child", ExpectedRevision: oldTrash.NodeRevision, ExpectedCollectionRevision: deleted.CollectionRevision})
	node, err = store.GetPersonalNode(ctx, "alice", "child")
	if err != nil || node.DeletedAt != nil || node.ParentID != "" {
		t.Fatalf("root fallback=%+v err=%v", node, err)
	}
}
