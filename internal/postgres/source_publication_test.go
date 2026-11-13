package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hhc/asset-api/internal/auditclient"
	"hhc/asset-api/internal/auditoutbox"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func insertLineSource(t *testing.T, db *sql.DB, id string, now time.Time) assets.Asset {
	t.Helper()
	insertAsset(t, db, id, assets.UploadCompleted, assets.ScanClean, assets.ProcessingNotRequired, now, time.Time{})
	source := assets.SourcePolicy{SourceReference: assets.SourceReference{Ref: "source_opaque_" + id, Revision: "revision_opaque_123", PublishedAt: now}, Version: 1, ExpiresAt: now.Add(assets.SourceLifetime)}
	data, _ := json.Marshal(source)
	if _, err := db.Exec(`UPDATE assets SET namespace='line.group.media-sync',owner_service='hhc-line-harness',security_profile='line-source-unscanned-v1',processing_profile='no-processing-v1',source_policy=$2,source_expires_at=$3,scan_status='not_required',visibility='restricted',etag='immutable',detected_mime_type='application/pdf',size_bytes=20 WHERE id=$1`, id, data, source.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	a, err := New(db).GetAsset(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestSourcePublicationStagedActivationAndACLRevision(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	a := insertLineSource(t, db, "publication", now)
	c, err := store.CreateCollection(ctx, assets.CreateCollectionInput{Namespace: "line.group.media-sync", Name: "group", CallerService: assets.LineHarnessCaller, IdempotencyKey: "collection", BindingGeneration: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if c.ACLRevision != 1 || c.SourcePolicyVersion != 1 || c.BindingGeneration != 1 {
		t.Fatalf("collection %+v", c)
	}
	input := assets.StageSourceItemInput{AddCollectionItemInput: assets.AddCollectionItemInput{CollectionID: c.ID, AssetID: a.ID, RemoteItemID: "opaque-item", DisplayName: "file.pdf", SourceRevision: "publication_revision_123", CallerService: assets.LineHarnessCaller, IdempotencyKey: "stage"}, BindingGeneration: 1, ExpectedACLRevision: 1}
	staged, err := store.StageSourceItem(ctx, input, now)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.StageSourceItem(ctx, input, now)
	if err != nil || replay.Item.ID != staged.Item.ID {
		t.Fatalf("stage replay %+v %v", replay, err)
	}
	if staged.Collection.Revision != c.Revision || staged.Item.PublicationState != "staged" {
		t.Fatalf("stage advanced cursor %+v", staged)
	}
	// Owner reconciliation is independent of content ACL and source gate availability.
	recovered, err := store.GetSourcePublication(ctx, c.ID, "stage", assets.LineHarnessCaller)
	if err != nil || recovered.Item.ID != staged.Item.ID {
		t.Fatalf("recover %+v %v", recovered, err)
	}
	activate := assets.ActivateSourceItemInput{CollectionID: c.ID, ItemID: staged.Item.ID, CallerService: assets.LineHarnessCaller, IdempotencyKey: "activate", BindingGeneration: 1, ExpectedACLRevision: 1}
	active, err := store.ActivateSourceItem(ctx, activate, now)
	if err != nil {
		t.Fatal(err)
	}
	if active.Item.PublicationState != "active" || active.Collection.Revision != c.Revision+1 || active.Item.CreatedRevision != active.Collection.Revision {
		t.Fatalf("activation %+v", active)
	}
	replay, err = store.ActivateSourceItem(ctx, activate, now)
	if err != nil || replay.Item.ID != active.Item.ID || replay.Collection.Revision != active.Collection.Revision {
		t.Fatalf("activate replay %+v %v", replay, err)
	}
	_, err = store.AddCollectionACL(ctx, assets.AddCollectionACLInput{CollectionID: c.ID, SubjectType: assets.SubjectUser, SubjectID: "reader", Permission: assets.PermissionRead, CallerService: assets.LineHarnessCaller, ActorUserID: "admin", RequestID: "request", IdempotencyKey: "acl", ExpectedACLRevision: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := store.GetCollectionSourcePolicy(ctx, c.ID)
	if err != nil || policy.ACLRevision != 2 {
		t.Fatalf("persisted ACL revision %+v %v", policy, err)
	}
	input.IdempotencyKey = "stale-stage"
	if _, err := store.StageSourceItem(ctx, input, now); err == nil {
		t.Fatal("stale approval staged")
	}
	input.CallerService = "hhc-line-function-bot"
	if _, err := store.StageSourceItem(ctx, input, now); err == nil {
		t.Fatal("legacy producer staged")
	}
}

func TestStagedSourceIsAbsentFromReaderChanges(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	a := insertLineSource(t, db, "hidden-publication", now)
	c, err := store.CreateCollection(ctx, assets.CreateCollectionInput{Namespace: "line.group.media-sync", Name: "group", CallerService: assets.LineHarnessCaller, IdempotencyKey: "collection", BindingGeneration: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.AddCollectionACL(ctx, assets.AddCollectionACLInput{CollectionID: c.ID, SubjectType: assets.SubjectUser, SubjectID: "reader", Permission: assets.PermissionRead, CallerService: assets.LineHarnessCaller, ActorUserID: "admin", RequestID: "request", IdempotencyKey: "acl", ExpectedACLRevision: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := store.StageSourceItem(ctx, assets.StageSourceItemInput{AddCollectionItemInput: assets.AddCollectionItemInput{CollectionID: c.ID, AssetID: a.ID, RemoteItemID: "opaque-item", DisplayName: "file.pdf", SourceRevision: "publication_revision_123", CallerService: assets.LineHarnessCaller, IdempotencyKey: "stage"}, BindingGeneration: 1, ExpectedACLRevision: 2}, now)
	if err != nil {
		t.Fatal(err)
	}
	subject := assets.CollectionSubject{UserID: "reader"}
	page, err := store.CollectionChanges(ctx, c.ID, "", subject)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("staged reset disclosure %+v %v", page, err)
	}
	page, err = store.CollectionChanges(ctx, c.ID, page.Cursor, subject)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("staged delta disclosure %+v %v", page, err)
	}
	active, err := store.ActivateSourceItem(ctx, assets.ActivateSourceItemInput{CollectionID: c.ID, ItemID: stage.Item.ID, CallerService: assets.LineHarnessCaller, IdempotencyKey: "activate", BindingGeneration: 1, ExpectedACLRevision: 2}, now)
	if err != nil {
		t.Fatal(err)
	}
	page, err = store.CollectionChanges(ctx, c.ID, page.Cursor, subject)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != active.Item.ID || !page.Items[0].SourceExpiresAt.Equal(a.Source.ExpiresAt) {
		t.Fatalf("activation missing %+v %v", page, err)
	}
}

func TestSourcePublicationRecoverySurvivesCollectionDeletion(t *testing.T) {
	store, c, item, _, now := sourceReaderFixture(t)
	ctx := context.Background()
	if _, err := store.DeleteCollection(ctx, assets.DeleteCollectionInput{CollectionID: c.ID, CallerService: assets.LineHarnessCaller, IdempotencyKey: "delete"}, now); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.GetSourcePublication(ctx, c.ID, "stage", assets.LineHarnessCaller)
	if err != nil || recovered.Collection.DeletedAt.IsZero() || recovered.Item.ID != item.ID || recovered.Item.DeletedRevision == 0 {
		t.Fatalf("recovery %+v %v", recovered, err)
	}
}

func TestSourceBindingCASRejectsCompetingSetupAndOldACL(t *testing.T) {
	store, c, _, _, now := sourceReaderFixture(t)
	ctx := context.Background()
	input := assets.BindSourceCollectionInput{CollectionID: c.ID, CallerService: assets.LineHarnessCaller, IdempotencyKey: "bind", ExpectedBindingGeneration: 1, BindingGeneration: 2, ExpectedACLRevision: 2}
	bound, err := store.BindSourceCollection(ctx, input, now)
	if err != nil || bound.BindingGeneration != 2 {
		t.Fatalf("binding %+v %v", bound, err)
	}
	replay, err := store.BindSourceCollection(ctx, input, now)
	if err != nil || replay.Revision != bound.Revision {
		t.Fatalf("binding replay %+v %v", replay, err)
	}
	input.IdempotencyKey = "competing"
	if _, err := store.BindSourceCollection(ctx, input, now); err == nil {
		t.Fatal("competing setup committed")
	}
	input.ExpectedBindingGeneration = 2
	input.BindingGeneration = 3
	input.ExpectedACLRevision = 1
	if _, err := store.BindSourceCollection(ctx, input, now); err == nil {
		t.Fatal("stale ACL committed")
	}
}

func TestSourceBindingConcurrentCAS(t *testing.T) {
	store, c, _, _, now := sourceReaderFixture(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	for n := 0; n < 2; n++ {
		go func(n int) {
			<-start
			_, err := store.BindSourceCollection(context.Background(), assets.BindSourceCollectionInput{CollectionID: c.ID, CallerService: assets.LineHarnessCaller, IdempotencyKey: fmt.Sprint("bind-", n), ExpectedBindingGeneration: 1, BindingGeneration: 2, ExpectedACLRevision: 2}, now)
			results <- err
		}(n)
	}
	close(start)
	first, second := <-results, <-results
	if !((first == nil && errors.Is(second, assets.ErrConflict)) || (second == nil && errors.Is(first, assets.ErrConflict))) {
		t.Fatalf("CAS results: %v, %v", first, second)
	}
}

func TestSourcePublicationAuditedTransaction(t *testing.T) {
	store, c, _, _, now := sourceReaderFixture(t)
	store.WithAudit(auditoutbox.New(store.db))
	asset := insertLineSource(t, store.db, "0123456789abcdef0123456789abcdef", now)
	input := assets.StageSourceItemInput{AddCollectionItemInput: assets.AddCollectionItemInput{CollectionID: c.ID, AssetID: asset.ID, RemoteItemID: "audit-item", DisplayName: "file.pdf", SourceRevision: "revision_opaque_123", CallerService: assets.LineHarnessCaller, IdempotencyKey: "audit-stage"}, BindingGeneration: 1, ExpectedACLRevision: 2}
	if _, err := store.StageSourceItem(context.Background(), input, now); err == nil {
		t.Fatal("missing audit actor committed")
	}
	if _, err := store.GetSourcePublication(context.Background(), c.ID, input.IdempotencyKey, assets.LineHarnessCaller); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("failed audit left publication: %v", err)
	}
	ctx := auditclient.WithProvenance(context.Background(), "service", assets.LineHarnessCaller, "audit-source-test")
	stage, err := store.StageSourceItem(ctx, input, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ActivateSourceItem(ctx, assets.ActivateSourceItemInput{CollectionID: c.ID, ItemID: stage.Item.ID, CallerService: assets.LineHarnessCaller, IdempotencyKey: "audit-activate", BindingGeneration: 1, ExpectedACLRevision: 2}, now)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM audit_outboxes`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("outbox %d %v", count, err)
	}
}

func TestLegacyBotCannotMutateHarnessCollection(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Now().UTC()
	c, err := store.CreateCollection(ctx, assets.CreateCollectionInput{Namespace: "line.group.media-sync", Name: "Harness group", CallerService: assets.LineHarnessCaller, IdempotencyKey: "new-owner", BindingGeneration: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	legacy := "hhc-line-function-bot"
	_, err = store.AddCollectionACL(ctx, assets.AddCollectionACLInput{CollectionID: c.ID, CallerService: legacy, ActorUserID: testAuditActorUserID, RequestID: testAuditRequestID, IdempotencyKey: "legacy-acl", ExpectedACLRevision: 1, SubjectType: assets.SubjectUser, SubjectID: testAuditActorUserID, Permission: assets.PermissionRead}, now)
	if !errors.Is(err, assets.ErrForbidden) {
		t.Fatalf("legacy ACL: %v", err)
	}
	_, err = store.DeleteCollection(ctx, assets.DeleteCollectionInput{CollectionID: c.ID, CallerService: legacy, IdempotencyKey: "legacy-delete"}, now)
	if !errors.Is(err, assets.ErrForbidden) {
		t.Fatalf("legacy delete: %v", err)
	}
	_, err = store.BindSourceCollection(ctx, assets.BindSourceCollectionInput{CollectionID: c.ID, CallerService: legacy, IdempotencyKey: "legacy-bind", ExpectedBindingGeneration: 1, BindingGeneration: 2, ExpectedACLRevision: 1}, now)
	if !errors.Is(err, assets.ErrForbidden) {
		t.Fatalf("legacy binding: %v", err)
	}
}

func TestHarnessCollectionProjectionBoundsCurrentACLWithoutHistory(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Now().UTC()
	var first string
	for i := 0; i < 25; i++ {
		c, err := store.CreateCollection(ctx, assets.CreateCollectionInput{Namespace: "line.group.media-sync", Name: "group", CallerService: assets.LineHarnessCaller, IdempotencyKey: fmt.Sprintf("projection-%d", i), BindingGeneration: 1}, now)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = c.ID
		}
		_, err = db.Exec(`INSERT INTO asset_collection_acl(id,collection_id,subject_type,subject_id,permission,created_at,revoked_at) SELECT md5($1::text || n::text),$1,'user',gen_random_uuid()::text,'read',$2,CASE WHEN n=1 THEN NULL ELSE $2::timestamptz END FROM generate_series(1,201) n`, c.ID, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	page, err := store.ListManagedCollections(ctx, assets.LineHarnessCaller, "", 25)
	if err != nil || len(page.Collections) != 25 {
		t.Fatalf("page %d %v", len(page.Collections), err)
	}
	for _, c := range page.Collections {
		if len(c.ACLs) != 0 {
			t.Fatal("collection picker exposed ACL history")
		}
	}
	encoded, err := json.Marshal(page)
	if err != nil || len(encoded) > 128*1024 {
		t.Fatalf("picker wire size=%d err=%v", len(encoded), err)
	}
	detail, err := store.GetManagedCollection(ctx, first, assets.LineHarnessCaller)
	if err != nil || len(detail.ACLs) != 1 || !detail.ACLs[0].RevokedAt.IsZero() {
		t.Fatalf("active projection count=%d err=%v", len(detail.ACLs), err)
	}
	// The bounded reader contract must reject overflow, never silently authorize a truncated audience.
	_, err = db.Exec(`INSERT INTO asset_collection_acl(id,collection_id,subject_type,subject_id,permission,created_at) SELECT md5('extra' || n::text),$1,'user',gen_random_uuid()::text,'read',$2 FROM generate_series(1,128) n`, first, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetManagedCollection(ctx, first, assets.LineHarnessCaller); !errors.Is(err, assets.ErrInvalidInput) {
		t.Fatalf("active overflow: %v", err)
	}
}
