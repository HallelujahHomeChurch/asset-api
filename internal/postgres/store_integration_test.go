package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestProcessingRetryClaimsDueUnlockedAssets(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "first", assets.UploadCompleted, assets.ScanClean, assets.ProcessingPending, now.Add(-time.Minute), time.Time{})
	insertAsset(t, db, "second", assets.UploadCompleted, assets.ScanClean, assets.ProcessingPending, now, time.Time{})

	lock, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err := lock.ExecContext(ctx, `SELECT id FROM assets WHERE id='first' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}

	claimed, ok, err := store.ClaimPendingProcessing(ctx, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if claimed.ID != "second" || claimed.ProcessingAttempts != 1 {
		t.Fatalf("claimed=%+v", claimed)
	}
	if err := store.ScheduleProcessingRetry(ctx, claimed.ID, claimed.ETag, claimed.ProcessingAttempts, "temporary", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.ClaimPendingProcessing(ctx, now.Add(30*time.Second), time.Minute); err != nil || ok {
		t.Fatalf("early claim: ok=%v err=%v", ok, err)
	}
	claimed, ok, err = store.ClaimPendingProcessing(ctx, now.Add(time.Minute), time.Minute)
	if err != nil || !ok || claimed.ID != "second" || claimed.ProcessingAttempts != 2 {
		t.Fatalf("retry claim: asset=%+v ok=%v err=%v", claimed, ok, err)
	}
}

func TestProcessingAttemptFencesStaleWorkersAndCompletionIsReentrant(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "fenced", assets.UploadCompleted, assets.ScanClean, assets.ProcessingPending, now, time.Time{})

	first, ok, err := store.ClaimPendingProcessing(ctx, now, time.Minute)
	if err != nil || !ok || first.ProcessingAttempts != 1 {
		t.Fatalf("first claim=%+v ok=%v err=%v", first, ok, err)
	}
	second, ok, err := store.ClaimPendingProcessing(ctx, now.Add(2*time.Minute), time.Minute)
	if err != nil || !ok || second.ProcessingAttempts != 2 {
		t.Fatalf("second claim=%+v ok=%v err=%v", second, ok, err)
	}
	firstDerivative := testDerivative("fenced", 1)
	if err := store.CompleteProcessing(ctx, "fenced", first.ETag, first.ProcessingAttempts, []assets.Derivative{firstDerivative}, now); err == nil {
		t.Fatal("stale completion succeeded")
	}
	if err := store.ScheduleProcessingRetry(ctx, "fenced", first.ETag, first.ProcessingAttempts, "stale", now, now); err == nil {
		t.Fatal("stale retry succeeded")
	}
	if err := store.FailProcessing(ctx, "fenced", first.ETag, first.ProcessingAttempts, "stale", now); err == nil {
		t.Fatal("stale failure succeeded")
	}

	secondDerivative := testDerivative("fenced", 2)
	if err := store.CompleteProcessing(ctx, "fenced", second.ETag, second.ProcessingAttempts, []assets.Derivative{secondDerivative}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteProcessing(ctx, "fenced", second.ETag, second.ProcessingAttempts, []assets.Derivative{secondDerivative}, now); err != nil {
		t.Fatalf("reentrant completion: %v", err)
	}
	stored, err := store.GetDerivative(ctx, "fenced", "small")
	if err != nil {
		t.Fatal(err)
	}
	if stored.ObjectKey != secondDerivative.ObjectKey {
		t.Fatalf("objectKey=%s", stored.ObjectKey)
	}
}

func TestFifthProcessingAttemptCrashTerminalizesAfterLease(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "fifth", assets.UploadCompleted, assets.ScanClean, assets.ProcessingPending, now, time.Time{})
	if _, err := db.Exec(`UPDATE assets SET processing_attempts=4 WHERE id='fifth'`); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimPendingProcessing(ctx, now, time.Minute)
	if err != nil || !ok || claimed.ProcessingAttempts != 5 {
		t.Fatalf("fifth claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	if _, ok, err := store.ClaimPendingProcessing(ctx, now.Add(2*time.Minute), time.Minute); err != nil || ok {
		t.Fatalf("post-lease claim: ok=%v err=%v", ok, err)
	}
	var status, details string
	if err := db.QueryRow(`SELECT processing_status,processing_error FROM assets WHERE id='fifth'`).Scan(&status, &details); err != nil {
		t.Fatal(err)
	}
	if status != string(assets.ProcessingFailed) || details == "" {
		t.Fatalf("status=%s details=%q", status, details)
	}
}

func TestConcurrentProcessingClaimsAreUnique(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	const count = 8
	for index := range count {
		insertAsset(t, db, fmt.Sprintf("asset-%d", index), assets.UploadCompleted, assets.ScanClean, assets.ProcessingPending, now, time.Time{})
	}

	start := make(chan struct{})
	claimed := make(chan string, count)
	failures := make(chan error, count)
	var workers sync.WaitGroup
	for range count {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			asset, ok, err := store.ClaimPendingProcessing(ctx, now, time.Minute)
			if err != nil {
				failures <- err
				return
			}
			if !ok {
				failures <- errors.New("no asset claimed")
				return
			}
			claimed <- asset.ID
		}()
	}
	close(start)
	workers.Wait()
	close(claimed)
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	unique := make(map[string]bool, count)
	for id := range claimed {
		if unique[id] {
			t.Errorf("asset claimed twice: %s", id)
		}
		unique[id] = true
	}
	if len(unique) != count {
		t.Fatalf("unique claims=%d", len(unique))
	}
}

func TestPurgedAssetsAreExcludedFromStateTransitions(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "purged", assets.UploadCompleted, assets.ScanClean, assets.ProcessingPending, now, now)

	if _, err := store.GetAsset(ctx, "purged"); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("GetAsset err=%v", err)
	}
	if _, ok, err := store.ClaimPendingProcessing(ctx, now, time.Minute); err != nil || ok {
		t.Fatalf("processing claim: ok=%v err=%v", ok, err)
	}
	if err := store.CompleteProcessing(ctx, "purged", "etag-purged", 1, nil, now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("CompleteProcessing err=%v", err)
	}
	if err := store.FailProcessing(ctx, "purged", "etag-purged", 1, "invalid", now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("FailProcessing err=%v", err)
	}
	if _, err := db.Exec(`UPDATE assets SET upload_status='created',scan_status='pending' WHERE id='purged'`); err != nil {
		t.Fatal(err)
	}
	session := assets.UploadSession{AssetID: "purged", Status: assets.UploadCreated}
	if _, err := db.Exec(`INSERT INTO upload_sessions(id,asset_id,idempotency_key,max_size_bytes,status,expires_at,created_at) VALUES('session','purged','key',1,'created',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteUpload(ctx, assets.Asset{ID: "purged"}, session); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("CompleteUpload err=%v", err)
	}
	if err := store.FailUpload(ctx, "purged", now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("FailUpload err=%v", err)
	}
	if err := store.ScheduleScanRetry(ctx, "purged", 1, "temporary", now.Add(time.Minute), now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("ScheduleScanRetry err=%v", err)
	}
	if _, ok, err := store.ClaimPendingScan(ctx, now, time.Minute); err != nil || ok {
		t.Fatalf("scan claim: ok=%v err=%v", ok, err)
	}
	if _, err := db.Exec(`UPDATE assets SET scan_status='failed' WHERE id='purged'`); err != nil {
		t.Fatal(err)
	}
	if err := store.RequeueFailedScan(ctx, "purged", "test", now); !errors.Is(err, assets.ErrInvalidInput) {
		t.Fatalf("RequeueFailedScan err=%v", err)
	}
}

func TestScanLeaseRejectsStaleWorkerWrites(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "scan-fence", assets.UploadCompleted, assets.ScanPending, assets.ProcessingPending, now, time.Time{})

	claimed, ok, err := store.ClaimPendingScan(ctx, now, time.Minute)
	if err != nil || !ok || claimed.ScanAttempts != 1 {
		t.Fatalf("claim: asset=%+v ok=%v err=%v", claimed, ok, err)
	}
	if _, err := db.Exec(`UPDATE assets SET scan_attempts=2,scan_claimed_until=$2 WHERE id=$1`, claimed.ID, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	result := assets.ScanResult{
		EventID:         "stale-scan",
		AssetID:         claimed.ID,
		Status:          assets.ScanClean,
		ETag:            claimed.ETag,
		ExpectedAttempt: claimed.ScanAttempts,
	}
	if _, err := store.ApplyScanResult(ctx, result, now); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("ApplyScanResult err=%v", err)
	}
	if err := store.ScheduleScanRetry(ctx, claimed.ID, claimed.ScanAttempts, "stale", now.Add(time.Minute), now); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("ScheduleScanRetry err=%v", err)
	}

	current, err := store.GetAsset(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ScanStatus != assets.ScanPending || current.ScanAttempts != 2 {
		t.Fatalf("current asset=%+v", current)
	}
}

func TestRetentionForeignKeysCascadeAndIndexesExist(t *testing.T) {
	db := integrationDB(t)
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "retained", assets.UploadCompleted, assets.ScanClean, assets.ProcessingNotRequired, now, time.Time{})
	statements := []string{
		`INSERT INTO upload_sessions(id,asset_id,idempotency_key,max_size_bytes,status,expires_at,created_at) VALUES('session','retained','upload-key',1,'completed',$1,$1)`,
		`INSERT INTO asset_grants(id,asset_id,subject_type,subject_id,permission,idempotency_key,expires_at,created_at) VALUES('grant','retained','public','*','read','grant-key',NULL,$1)`,
		`INSERT INTO asset_scan_events(event_id,asset_id,status,created_at) VALUES('event','retained','clean',$1)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`DELETE FROM assets WHERE id='retained'`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"upload_sessions", "asset_grants", "asset_scan_events"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s rows=%d", table, count)
		}
	}
	for _, index := range []string{"asset_grants_retention_idx", "asset_scan_events_retention_idx"} {
		var exists bool
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname=current_schema() AND indexname=$1)`, index).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("missing index %s", index)
		}
	}
}

func TestDeleteExpiredPurgeIsBoundedAndPreservesRecentOrActiveAssets(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	cutoff := now.Add(-180 * 24 * time.Hour)
	insertAsset(t, db, "old-1", assets.UploadCompleted, assets.ScanClean, assets.ProcessingNotRequired, cutoff.Add(-2*time.Hour), cutoff.Add(-2*time.Hour))
	insertAsset(t, db, "old-2", assets.UploadCompleted, assets.ScanClean, assets.ProcessingNotRequired, cutoff.Add(-time.Hour), cutoff.Add(-time.Hour))
	insertAsset(t, db, "recent", assets.UploadCompleted, assets.ScanClean, assets.ProcessingNotRequired, cutoff, cutoff)
	insertAsset(t, db, "active", assets.UploadCompleted, assets.ScanClean, assets.ProcessingNotRequired, cutoff.Add(-time.Hour), time.Time{})
	for _, assetID := range []string{"old-1", "old-2"} {
		if _, err := db.Exec(`INSERT INTO asset_grants(id,asset_id,subject_type,subject_id,permission,idempotency_key,created_at) VALUES($1,$2,'public','*','read',$1,$3)`, "grant-"+assetID, assetID, now); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := store.DeleteExpiredPurge(ctx, cutoff, 1)
	if err != nil || deleted != 1 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
	for _, id := range []string{"old-2", "recent", "active"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM assets WHERE id=$1`, id).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s count=%d err=%v", id, count, err)
		}
	}
	var oldAsset, oldGrant int
	if err := db.QueryRow(`SELECT count(*) FROM assets WHERE id='old-1'`).Scan(&oldAsset); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM asset_grants WHERE asset_id='old-1'`).Scan(&oldGrant); err != nil {
		t.Fatal(err)
	}
	if oldAsset != 0 || oldGrant != 0 {
		t.Fatalf("old asset=%d grant=%d", oldAsset, oldGrant)
	}

	var retentionIndex bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname=current_schema() AND indexname='assets_purged_retention_idx')`).Scan(&retentionIndex); err != nil {
		t.Fatal(err)
	}
	if !retentionIndex {
		t.Fatal("missing assets_purged_retention_idx")
	}
}

func TestAssetStateConstraintsRejectUnknownValues(t *testing.T) {
	db := integrationDB(t)
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "state-constraints", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})

	for _, statement := range []string{
		`UPDATE assets SET upload_status='unknown' WHERE id='state-constraints'`,
		`UPDATE assets SET scan_status='unknown' WHERE id='state-constraints'`,
		`UPDATE assets SET processing_status='unknown' WHERE id='state-constraints'`,
	} {
		if _, err := db.Exec(statement); err == nil {
			t.Fatalf("statement unexpectedly succeeded: %s", statement)
		}
	}
}

func TestOperationsIncludesProcessingBacklog(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	oldest := now.Add(-time.Hour)
	insertAsset(t, db, "processing-pending", assets.UploadCompleted, assets.ScanClean, assets.ProcessingPending, oldest, time.Time{})
	insertAsset(t, db, "processing-failed", assets.UploadCompleted, assets.ScanClean, assets.ProcessingFailed, now, time.Time{})

	operations, err := store.GetOperations(ctx, now)

	if err != nil {
		t.Fatal(err)
	}
	if operations.ProcessingPending != 1 || operations.ProcessingFailed != 1 || !operations.OldestProcessingPending.Equal(oldest) {
		t.Fatalf("operations=%+v", operations)
	}
}

func TestPurgeIncludesAttemptSpecificDerivativeKeys(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "purge-keys", assets.UploadCompleted, assets.ScanClean, assets.ProcessingFailed, now.Add(-8*24*time.Hour), time.Time{})

	candidate, ok, err := store.ClaimPurge(ctx, now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim purge: ok=%v err=%v", ok, err)
	}
	want := "assets/derivatives/attempt-5/large.jpg"
	for _, key := range candidate.Keys {
		if key == want {
			return
		}
	}
	t.Fatalf("missing purge key %s: %v", want, candidate.Keys)
}

func integrationDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	adminConfig, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	adminDB := stdlib.OpenDB(*adminConfig)
	t.Cleanup(func() { adminDB.Close() })
	schema := fmt.Sprintf("asset_task3_%d", time.Now().UnixNano())
	if _, err := adminDB.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = adminDB.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`) })

	testConfig := adminConfig.Copy()
	testConfig.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*testConfig)
	t.Cleanup(func() { db.Close() })
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}

func insertAsset(t *testing.T, db *sql.DB, id string, upload assets.UploadStatus, scan assets.ScanStatus, processing assets.ProcessingStatus, updatedAt, purgedAt time.Time) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO assets(
			id,namespace,owner_service,owner_type,owner_id,object_key,expected_mime_type,
			upload_status,scan_status,processing_status,visibility,created_at,updated_at,purged_at,etag
		) VALUES($1,'cms.news.cover','test','item',$1,$2,'image/png',$3,$4,$5,'public',$6,$6,NULLIF($7,'0001-01-01'::timestamptz),$8)`,
		id, "assets/"+id, upload, scan, processing, updatedAt, purgedAt, "etag-"+id)
	if err != nil {
		t.Fatal(err)
	}
}

func testDerivative(assetID string, attempt int) assets.Derivative {
	return assets.Derivative{
		AssetID:   assetID,
		Variant:   "small",
		ObjectKey: fmt.Sprintf("assets/%s/derivatives/attempt-%d/small.jpg", assetID, attempt),
		MIMEType:  "image/jpeg",
		Width:     480,
		Height:    240,
		SizeBytes: 10,
		ETag:      fmt.Sprintf("attempt-%d", attempt),
		CreatedAt: time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
	}
}
