package postgres

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/lifecycle"
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

func TestClaimProcessingTargetsOnlyTheRequestedAssetVersion(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "exact", assets.UploadCompleted, assets.ScanClean, assets.ProcessingPending, now, time.Time{})
	insertAsset(t, db, "other", assets.UploadCompleted, assets.ScanClean, assets.ProcessingPending, now, time.Time{})
	if _, err := db.Exec(`UPDATE assets SET detected_mime_type='image/png' WHERE id IN ('exact','other')`); err != nil {
		t.Fatal(err)
	}

	claimed, ok, err := store.ClaimProcessing(ctx, "exact", "etag-exact", now, time.Minute)
	if err != nil || !ok || claimed.ID != "exact" || claimed.ProcessingAttempts != 1 {
		t.Fatalf("claimed=%+v ok=%v err=%v", claimed, ok, err)
	}
	if _, ok, err := store.ClaimProcessing(ctx, "exact", "etag-exact", now, time.Minute); !errors.Is(err, assets.ErrConflict) || ok {
		t.Fatalf("busy claim: ok=%v err=%v", ok, err)
	}
	var otherAttempts int
	if err := db.QueryRow(`SELECT processing_attempts FROM assets WHERE id='other'`).Scan(&otherAttempts); err != nil {
		t.Fatal(err)
	}
	if otherAttempts != 0 {
		t.Fatalf("other attempts=%d", otherAttempts)
	}
}

func TestClaimProcessingRejectsAlreadyCompleteStaleAndIneligibleAssets(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		id         string
		scan       assets.ScanStatus
		processing assets.ProcessingStatus
		mime       string
		etag       string
		deleted    bool
	}{
		{id: "ready", scan: assets.ScanClean, processing: assets.ProcessingReady, mime: "image/png", etag: "etag-ready"},
		{id: "stale-version", scan: assets.ScanClean, processing: assets.ProcessingPending, mime: "image/png", etag: "stale"},
		{id: "non-clean", scan: assets.ScanPending, processing: assets.ProcessingPending, mime: "image/png", etag: "etag-non-clean"},
		{id: "deleted", scan: assets.ScanClean, processing: assets.ProcessingPending, mime: "image/png", etag: "etag-deleted", deleted: true},
		{id: "unsupported", scan: assets.ScanClean, processing: assets.ProcessingPending, mime: "application/pdf", etag: "etag-unsupported"},
	}
	for _, test := range tests {
		t.Run(test.id, func(t *testing.T) {
			insertAsset(t, db, test.id, assets.UploadCompleted, test.scan, test.processing, now, time.Time{})
			if _, err := db.Exec(`UPDATE assets SET detected_mime_type=$2 WHERE id=$1`, test.id, test.mime); err != nil {
				t.Fatal(err)
			}
			if test.deleted {
				if _, err := db.Exec(`UPDATE assets SET deleted_at=$2 WHERE id=$1`, test.id, now); err != nil {
					t.Fatal(err)
				}
			}
			if claimed, ok, err := store.ClaimProcessing(ctx, test.id, test.etag, now, time.Minute); err != nil || ok {
				t.Fatalf("claimed=%+v ok=%v err=%v", claimed, ok, err)
			}
		})
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
	if err := store.CompleteUpload(ctx, assets.Asset{ID: "purged", ETag: "etag-purged"}, session, assets.ScanRequest{EventID: "purged-event", AssetID: "purged", ETag: "etag-purged", CreatedAt: now}); !errors.Is(err, assets.ErrNotFound) {
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
	if err := store.RequeueFailedScan(ctx, "purged", "test", assets.ScanRequest{EventID: "requeue-purged", AssetID: "purged", ETag: "etag-purged", CreatedAt: now}, now); !errors.Is(err, assets.ErrInvalidInput) {
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
		EventID:         claimed.ScanEventID,
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

func TestApplyScanResultCreatesOneDerivativeOutboxRow(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "derivative-clean", assets.UploadCompleted, assets.ScanPending, assets.ProcessingPending, now, time.Time{})
	if _, err := db.Exec(`UPDATE assets SET detected_mime_type='image/png' WHERE id='derivative-clean'`); err != nil {
		t.Fatal(err)
	}
	result := assets.ScanResult{EventID: "event-derivative-clean", AssetID: "derivative-clean", Status: assets.ScanClean, ETag: "etag-derivative-clean"}

	applied, err := store.ApplyScanResult(ctx, result, now)
	if err != nil || !applied {
		t.Fatalf("apply: applied=%v err=%v", applied, err)
	}
	if applied, err = store.ApplyScanResult(ctx, result, now); err != nil || applied {
		t.Fatalf("replay: applied=%v err=%v", applied, err)
	}
	var count int
	var assetID, etag string
	if err := db.QueryRow(`SELECT COUNT(*),MIN(asset_id),MIN(asset_etag) FROM asset_derivative_outbox WHERE asset_id='derivative-clean'`).Scan(&count, &assetID, &etag); err != nil {
		t.Fatal(err)
	}
	if count != 1 || assetID != "derivative-clean" || etag != "etag-derivative-clean" {
		t.Fatalf("count=%d assetID=%q etag=%q", count, assetID, etag)
	}
}

func TestApplyScanResultSkipsIneligibleDerivativeOutboxRows(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		id        string
		status    assets.ScanStatus
		mime      string
		etag      string
		deleted   bool
		wantError bool
	}{
		{id: "infected", status: assets.ScanInfected, mime: "image/png", etag: "etag-infected"},
		{id: "failed", status: assets.ScanFailed, mime: "image/png", etag: "etag-failed"},
		{id: "stale", status: assets.ScanClean, mime: "image/png", etag: "stale-etag", wantError: true},
		{id: "non-image", status: assets.ScanClean, mime: "application/pdf", etag: "etag-non-image"},
		{id: "rejected", status: assets.ScanClean, mime: "image/png", etag: "etag-rejected", deleted: true, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.id, func(t *testing.T) {
			insertAsset(t, db, test.id, assets.UploadCompleted, assets.ScanPending, assets.ProcessingPending, now, time.Time{})
			if _, err := db.Exec(`UPDATE assets SET detected_mime_type=$2 WHERE id=$1`, test.id, test.mime); err != nil {
				t.Fatal(err)
			}
			if test.deleted {
				if _, err := db.Exec(`UPDATE assets SET deleted_at=$2 WHERE id=$1`, test.id, now); err != nil {
					t.Fatal(err)
				}
			}
			_, err := store.ApplyScanResult(ctx, assets.ScanResult{EventID: "event-" + test.id, AssetID: test.id, Status: test.status, ETag: test.etag}, now)
			if (err != nil) != test.wantError {
				t.Fatalf("err=%v", err)
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM asset_derivative_outbox WHERE asset_id=$1`, test.id).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("outbox rows=%d", count)
			}
		})
	}
}

func TestDerivativeOutboxMigrationBackfillsCleanPendingImages(t *testing.T) {
	db := integrationDB(t)
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`DROP TABLE asset_derivative_outbox`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version='sql/014_asset_derivative_outbox.sql'`); err != nil {
		t.Fatal(err)
	}
	insertAsset(t, db, "derivative-backfill", assets.UploadCompleted, assets.ScanClean, assets.ProcessingPending, now, time.Time{})
	if _, err := db.Exec(`UPDATE assets SET detected_mime_type='image/webp' WHERE id='derivative-backfill'`); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM asset_derivative_outbox WHERE asset_id='derivative-backfill' AND asset_etag='etag-derivative-backfill' AND delivered_at IS NULL`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("backfill rows=%d", count)
	}
}

func TestDerivativeOutboxClaimRetryAndDeliveryAreFenced(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "derivative-dispatch", assets.UploadCompleted, assets.ScanClean, assets.ProcessingPending, now, time.Time{})
	if _, err := db.Exec(`INSERT INTO asset_derivative_outbox(event_id,asset_id,asset_etag,available_at,created_at) VALUES('derivative-event','derivative-dispatch','etag-derivative-dispatch',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	first, ok, err := store.ClaimDerivativeRequest(ctx, now, time.Minute)
	if err != nil || !ok || first.Attempts != 1 {
		t.Fatalf("first=%+v ok=%v err=%v", first, ok, err)
	}
	if _, ok, err := store.ClaimDerivativeRequest(ctx, now, time.Minute); err != nil || ok {
		t.Fatalf("concurrent claim: ok=%v err=%v", ok, err)
	}
	if err := store.ScheduleDerivativeRequestRetry(ctx, first.EventID, first.Attempts, "temporary", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	second, ok, err := store.ClaimDerivativeRequest(ctx, now.Add(time.Minute), time.Minute)
	if err != nil || !ok || second.Attempts != 2 {
		t.Fatalf("second=%+v ok=%v err=%v", second, ok, err)
	}
	if err := store.MarkDerivativeRequestDelivered(ctx, first.EventID, first.Attempts, now); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("stale mark err=%v", err)
	}
	if err := store.MarkDerivativeRequestDelivered(ctx, second.EventID, second.Attempts, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.ClaimDerivativeRequest(ctx, now.Add(2*time.Minute), time.Minute); err != nil || ok {
		t.Fatalf("delivered claim: ok=%v err=%v", ok, err)
	}
}

func TestApplyScanResultRollsBackWhenDerivativeOutboxInsertFails(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "derivative-rollback", assets.UploadCompleted, assets.ScanPending, assets.ProcessingPending, now, time.Time{})
	if _, err := db.Exec(`UPDATE assets SET detected_mime_type='image/jpeg' WHERE id='derivative-rollback'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE FUNCTION reject_derivative_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'rejected'; END $$; CREATE TRIGGER reject_derivative_outbox BEFORE INSERT ON asset_derivative_outbox FOR EACH ROW EXECUTE FUNCTION reject_derivative_outbox()`); err != nil {
		t.Fatal(err)
	}
	result := assets.ScanResult{EventID: "event-derivative-rollback", AssetID: "derivative-rollback", Status: assets.ScanClean, ETag: "etag-derivative-rollback"}
	if _, err := store.ApplyScanResult(ctx, result, now); err == nil {
		t.Fatal("scan result succeeded")
	}
	var status string
	var events int
	if err := db.QueryRow(`SELECT scan_status FROM assets WHERE id='derivative-rollback'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM asset_scan_events WHERE event_id='event-derivative-rollback'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if status != string(assets.ScanPending) || events != 0 {
		t.Fatalf("status=%q scan events=%d", status, events)
	}
}

func TestQueueScanClaimFencesEventAndReportsBusy(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "queue-fence", assets.UploadCompleted, assets.ScanPending, assets.ProcessingPending, now, time.Time{})

	if _, state, err := store.ClaimAssetScan(ctx, "stale-event", "queue-fence", "etag-queue-fence", now, time.Minute); err != nil || state != assets.ScanTerminal {
		t.Fatalf("stale claim state=%q err=%v", state, err)
	}
	claimed, state, err := store.ClaimAssetScan(ctx, "event-queue-fence", "queue-fence", "etag-queue-fence", now, time.Minute)
	if err != nil || state != assets.ScanClaimed || claimed.ScanAttempts != 1 {
		t.Fatalf("claim=%+v state=%q err=%v", claimed, state, err)
	}
	if _, state, err := store.ClaimAssetScan(ctx, "event-queue-fence", "queue-fence", "etag-queue-fence", now, time.Minute); err != nil || state != assets.ScanBusy {
		t.Fatalf("busy state=%q err=%v", state, err)
	}
}

func TestTerminalScanAndPoisonRecordCommitTogether(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "poison", assets.UploadCompleted, assets.ScanPending, assets.ProcessingPending, now, time.Time{})
	claimed, state, err := store.ClaimAssetScan(ctx, "event-poison", "poison", "etag-poison", now, time.Minute)
	if err != nil || state != assets.ScanClaimed {
		t.Fatalf("claim state=%q err=%v", state, err)
	}
	result := assets.ScanResult{EventID: "event-poison", AssetID: "poison", ETag: "etag-poison", ExpectedAttempt: claimed.ScanAttempts, Status: assets.ScanFailed, FailureCategory: "retry_exhausted", Signature: "sig-1"}
	poison := assets.ScanPoison{PoisonID: "message:retry", EventID: "event-poison", AssetID: "poison", ETag: "etag-poison", Reason: "retry_exhausted", DequeueCount: 5, SourceMessageID: "message", BodySHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	shouldForward, err := store.FailScanToPoison(ctx, result, poison, now)
	if err != nil || !shouldForward {
		t.Fatalf("fail-to-poison forward=%v err=%v", shouldForward, err)
	}
	var status string
	var count int
	if err := db.QueryRow(`SELECT scan_status FROM assets WHERE id='poison'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM asset_scan_poison_events WHERE poison_id='message:retry'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || count != 1 {
		t.Fatalf("status=%q poison count=%d", status, count)
	}
	if err := store.MarkScanPoisonForwarded(ctx, poison.PoisonID, now); err != nil {
		t.Fatal(err)
	}
	shouldForward, err = store.FailScanToPoison(ctx, result, poison, now)
	if err != nil || shouldForward {
		t.Fatalf("duplicate forward=%v err=%v", shouldForward, err)
	}
	replay := assets.ScanRequest{EventID: "event-poison-replay", AssetID: "poison", ETag: "etag-poison", CreatedAt: now.Add(time.Minute)}
	if err := store.RequeueFailedScan(ctx, "poison", "test", replay, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var replayEvent string
	var replayed bool
	if err := db.QueryRow(`SELECT replay_event_id,replayed_at IS NOT NULL FROM asset_scan_poison_events WHERE poison_id='message:retry'`).Scan(&replayEvent, &replayed); err != nil {
		t.Fatal(err)
	}
	if replayEvent != replay.EventID || !replayed {
		t.Fatalf("replay event=%q replayed=%v", replayEvent, replayed)
	}
}

func TestCompleteUploadCommitsOneScanRequestWithAssetState(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "outbox", assets.UploadCreated, assets.ScanPending, assets.ProcessingNotRequired, now, time.Time{})
	if _, err := db.Exec(`INSERT INTO upload_sessions(id,asset_id,idempotency_key,max_size_bytes,status,expires_at,created_at) VALUES('outbox-session','outbox','outbox-key',10,'created',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	asset := assets.Asset{ID: "outbox", ETag: "etag-1", DetectedMIMEType: "application/pdf", SizeBytes: 10, ChecksumSHA256: "checksum", UploadStatus: assets.UploadCompleted, ScanStatus: assets.ScanPending, UpdatedAt: now}
	session := assets.UploadSession{AssetID: "outbox", Status: assets.UploadCompleted, CompletedAt: now}
	request := assets.ScanRequest{EventID: "event-1", AssetID: "outbox", ETag: "etag-1", CreatedAt: now}

	if err := store.CompleteUpload(ctx, asset, session, request); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteUpload(ctx, asset, session, assets.ScanRequest{EventID: "event-2", AssetID: "outbox", ETag: "etag-1", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	var status, eventID string
	var count int
	if err := db.QueryRow(`SELECT upload_status FROM assets WHERE id='outbox'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*),MIN(event_id) FROM asset_scan_outbox WHERE asset_id='outbox'`).Scan(&count, &eventID); err != nil {
		t.Fatal(err)
	}
	if status != string(assets.UploadCompleted) || count != 1 || eventID != "event-1" {
		t.Fatalf("status=%q count=%d eventID=%q", status, count, eventID)
	}
}

func TestCompleteUploadRollsBackWhenScanRequestCannotBeWritten(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "outbox-rollback", assets.UploadCreated, assets.ScanPending, assets.ProcessingNotRequired, now, time.Time{})
	if _, err := db.Exec(`INSERT INTO upload_sessions(id,asset_id,idempotency_key,max_size_bytes,status,expires_at,created_at) VALUES('rollback-session','outbox-rollback','rollback-key',10,'created',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	asset := assets.Asset{ID: "outbox-rollback", ETag: "etag-1", DetectedMIMEType: "application/pdf", SizeBytes: 10, ChecksumSHA256: "checksum", UploadStatus: assets.UploadCompleted, ScanStatus: assets.ScanPending, UpdatedAt: now}
	session := assets.UploadSession{AssetID: "outbox-rollback", Status: assets.UploadCompleted, CompletedAt: now}
	if _, err := db.Exec(`INSERT INTO asset_scan_outbox(event_id,asset_id,asset_etag,available_at,created_at) VALUES('duplicate-event','outbox-rollback','old-etag',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}

	if err := store.CompleteUpload(ctx, asset, session, assets.ScanRequest{EventID: "duplicate-event", AssetID: asset.ID, ETag: asset.ETag, CreatedAt: now}); err == nil {
		t.Fatal("complete upload succeeded with duplicate event ID")
	}
	var status string
	if err := db.QueryRow(`SELECT upload_status FROM assets WHERE id='outbox-rollback'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(assets.UploadCreated) {
		t.Fatalf("status=%q", status)
	}
}

func TestScanRequestLeaseFencesStalePublisher(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "outbox-fence", assets.UploadCompleted, assets.ScanPending, assets.ProcessingNotRequired, now, time.Time{})
	if _, err := db.Exec(`INSERT INTO asset_scan_outbox(event_id,asset_id,asset_etag,available_at,created_at) VALUES('fenced-event','outbox-fence','etag-outbox-fence',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	first, ok, err := store.ClaimScanRequest(ctx, now, time.Minute)
	if err != nil || !ok || first.Attempts != 1 {
		t.Fatalf("first claim=%+v ok=%v err=%v", first, ok, err)
	}
	second, ok, err := store.ClaimScanRequest(ctx, now.Add(2*time.Minute), time.Minute)
	if err != nil || !ok || second.Attempts != 2 {
		t.Fatalf("second claim=%+v ok=%v err=%v", second, ok, err)
	}
	if err := store.MarkScanRequestDelivered(ctx, first.EventID, first.Attempts, now); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("stale mark error=%v", err)
	}
	if err := store.ScheduleScanRequestRetry(ctx, first.EventID, first.Attempts, "stale", now.Add(time.Minute), now); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("stale retry error=%v", err)
	}
	if err := store.MarkScanRequestDelivered(ctx, second.EventID, second.Attempts, now); err != nil {
		t.Fatal(err)
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

func TestCollectionSchemaDefaultsAndRevisionConstraints(t *testing.T) {
	db := integrationDB(t)
	now := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)

	if _, err := db.Exec(`INSERT INTO asset_collections(id,namespace,name,created_by_service,created_at,updated_at) VALUES('collection','line.group.media-sync','Media','hhc-line-function-bot',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := db.QueryRow(`SELECT revision FROM asset_collections WHERE id='collection'`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if revision != 1 {
		t.Fatalf("revision=%d", revision)
	}
	if _, err := db.Exec(`UPDATE asset_collections SET revision=0 WHERE id='collection'`); err == nil {
		t.Fatal("zero collection revision was accepted")
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES('item-zero','collection','remote-zero','Zero','source',0,false,0,$1,$1)`, now); err == nil {
		t.Fatal("zero item revision was accepted")
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,remote_item_id,display_name,source_revision,created_revision,deleted_revision,retention_exempt,updated_revision,created_at,updated_at,deleted_at) VALUES('item-reversed','collection','remote-reversed','Reversed','source',3,2,false,3,$1,$1,$1)`, now); err == nil {
		t.Fatal("deleted revision before created revision was accepted")
	}
}

func TestCollectionSchemaStableItemOccurrencesAndActiveUniqueness(t *testing.T) {
	db := integrationDB(t)
	now := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "collection-asset-1", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	insertAsset(t, db, "collection-asset-2", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	insertCollection(t, db, "collection", now)

	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES('','collection','collection-asset-1','remote-blank','Blank','source',1,false,1,$1,$1)`, now); err == nil {
		t.Fatal("blank item occurrence ID was accepted")
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES('item-1','collection','collection-asset-1','remote-1','One','source-1',1,false,1,$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES('duplicate-asset','collection','collection-asset-1','remote-2','Duplicate asset','source-2',2,false,2,$1,$1)`, now); err == nil {
		t.Fatal("duplicate active asset membership was accepted")
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES('duplicate-remote','collection','collection-asset-2','remote-1','Duplicate remote','source-2',2,false,2,$1,$1)`, now); err == nil {
		t.Fatal("duplicate active remote item was accepted")
	}
	if _, err := db.Exec(`UPDATE asset_collection_items SET deleted_revision=2,deleted_at=$1 WHERE id='item-1'`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES('item-2','collection','collection-asset-1','remote-1','One again','source-2',3,false,3,$1,$1)`, now); err != nil {
		t.Fatalf("re-add remote item: %v", err)
	}
	var oldDeletedRevision sql.NullInt64
	var activeID string
	if err := db.QueryRow(`SELECT deleted_revision FROM asset_collection_items WHERE id='item-1'`).Scan(&oldDeletedRevision); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT id FROM asset_collection_items WHERE collection_id='collection' AND remote_item_id='remote-1' AND deleted_revision IS NULL`).Scan(&activeID); err != nil {
		t.Fatal(err)
	}
	if !oldDeletedRevision.Valid || oldDeletedRevision.Int64 != 2 || activeID != "item-2" {
		t.Fatalf("oldDeletedRevision=%v activeID=%q", oldDeletedRevision, activeID)
	}
}

func TestCollectionSchemaACLConstraintsAndActiveUniqueness(t *testing.T) {
	db := integrationDB(t)
	now := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	insertCollection(t, db, "collection", now)

	for _, statement := range []string{
		`INSERT INTO asset_collection_acl(id,collection_id,subject_type,subject_id,permission,created_at) VALUES('invalid-type','collection','service','subject','read',$1)`,
		`INSERT INTO asset_collection_acl(id,collection_id,subject_type,subject_id,permission,created_at) VALUES('invalid-permission','collection','user','subject','write',$1)`,
	} {
		if _, err := db.Exec(statement, now); err == nil {
			t.Fatalf("invalid ACL was accepted: %s", statement)
		}
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_acl(id,collection_id,subject_type,subject_id,permission,created_at) VALUES('acl-1','collection','user','subject','read',$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_acl(id,collection_id,subject_type,subject_id,permission,created_at) VALUES('acl-duplicate','collection','user','subject','read',$1)`, now); err == nil {
		t.Fatal("duplicate active ACL was accepted")
	}
	if _, err := db.Exec(`UPDATE asset_collection_acl SET revoked_at=$1 WHERE id='acl-1'`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_acl(id,collection_id,subject_type,subject_id,permission,created_at) VALUES('acl-2','collection','user','subject','read',$1)`, now); err != nil {
		t.Fatalf("re-add revoked ACL: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_acl(id,collection_id,subject_type,subject_id,permission,created_at) VALUES('acl-role','collection','role','media_sync_user','read',$1)`, now); err != nil {
		t.Fatalf("role ACL: %v", err)
	}
}

func TestCollectionSchemaMutationClaimsAndTicketScope(t *testing.T) {
	db := integrationDB(t)
	now := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "ticket-asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	insertCollection(t, db, "collection", now)
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES('ticket-item','collection','ticket-asset','remote-ticket','Ticket','source',1,false,1,$1,$1)`, now); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(`INSERT INTO asset_collection_mutations(caller_service,operation,idempotency_key,request_fingerprint,response_json,created_at) VALUES('helper','create','key','fingerprint',NULL,$1)`, now); err != nil {
		t.Fatal(err)
	}
	var responseJSON []byte
	if err := db.QueryRow(`SELECT response_json FROM asset_collection_mutations WHERE caller_service='helper' AND operation='create' AND idempotency_key='key'`).Scan(&responseJSON); err != nil {
		t.Fatal(err)
	}
	if responseJSON != nil {
		t.Fatalf("response_json=%q", responseJSON)
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_mutations(caller_service,operation,idempotency_key,request_fingerprint,created_at) VALUES('helper','create','key','other',$1)`, now); err == nil {
		t.Fatal("duplicate mutation claim was accepted")
	}

	validHash := strings.Repeat("a", 64)
	if _, err := db.Exec(`INSERT INTO asset_content_tickets(token_hash,collection_id,collection_item_id,asset_etag,user_id,roles,expires_at,created_at) VALUES($1,'collection','ticket-item','etag-ticket','user',ARRAY['media_sync_user']::text[],$2,$3)`, validHash, now.Add(5*time.Minute), now); err != nil {
		t.Fatal(err)
	}
	var rolesType string
	if err := db.QueryRow(`SELECT pg_typeof(roles)::text FROM asset_content_tickets WHERE token_hash=$1`, validHash).Scan(&rolesType); err != nil {
		t.Fatal(err)
	}
	if rolesType != "text[]" {
		t.Fatalf("roles type=%q", rolesType)
	}
	insertCollection(t, db, "other-collection", now)
	if _, err := db.Exec(`INSERT INTO asset_content_tickets(token_hash,collection_id,collection_item_id,asset_etag,user_id,roles,expires_at,created_at) VALUES($1,'other-collection','ticket-item','etag-ticket','user',ARRAY[]::text[],$2,$3)`, strings.Repeat("d", 64), now.Add(time.Minute), now); err == nil {
		t.Fatal("ticket referencing an item from another collection was accepted")
	}
	for _, tokenHash := range []string{strings.Repeat("A", 64), strings.Repeat("a", 63), strings.Repeat("g", 64)} {
		if _, err := db.Exec(`INSERT INTO asset_content_tickets(token_hash,collection_id,collection_item_id,asset_etag,user_id,roles,expires_at,created_at) VALUES($1,'collection','ticket-item','etag-ticket','user',ARRAY[]::text[],$2,$3)`, tokenHash, now.Add(time.Minute), now); err == nil {
			t.Fatalf("invalid token hash was accepted: %q", tokenHash)
		}
	}
	if _, err := db.Exec(`INSERT INTO asset_content_tickets(token_hash,collection_id,collection_item_id,asset_etag,user_id,roles,expires_at,created_at) VALUES($1,'collection','missing-item','etag-ticket','user',ARRAY[]::text[],$2,$3)`, strings.Repeat("b", 64), now.Add(time.Minute), now); err == nil {
		t.Fatal("ticket without a concrete item occurrence was accepted")
	}
}

func TestCollectionSchemaAssetRetentionPreservesItemHistory(t *testing.T) {
	db := integrationDB(t)
	now := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "retained-collection-asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	insertCollection(t, db, "collection", now)
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,deleted_revision,retention_exempt,updated_revision,created_at,updated_at,deleted_at) VALUES('retained-item','collection','retained-collection-asset','remote-retained','Retained','source',1,2,false,1,$1,$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM assets WHERE id='retained-collection-asset'`); err != nil {
		t.Fatalf("retention delete: %v", err)
	}
	var assetID sql.NullString
	var itemID, remoteItemID string
	var deletedRevision int64
	if err := db.QueryRow(`SELECT id,asset_id,remote_item_id,deleted_revision FROM asset_collection_items WHERE id='retained-item'`).Scan(&itemID, &assetID, &remoteItemID, &deletedRevision); err != nil {
		t.Fatal(err)
	}
	if itemID != "retained-item" || assetID.Valid || remoteItemID != "remote-retained" || deletedRevision != 2 {
		t.Fatalf("itemID=%q assetID=%v remoteItemID=%q deletedRevision=%d", itemID, assetID, remoteItemID, deletedRevision)
	}
}

func TestCollectionManagementDefaults(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)

	collection, err := store.CreateCollection(ctx, assets.CreateCollectionInput{
		Namespace:      "namespace",
		Name:           "Managed media",
		CallerService:  "helper",
		IdempotencyKey: "collection-management-defaults",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if collection.RetentionDays != 14 {
		t.Fatalf("retention days = %d, want 14", collection.RetentionDays)
	}

	insertAsset(t, db, "management-default-asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	if _, err := db.Exec("UPDATE assets SET namespace='namespace',owner_service='helper' WHERE id='management-default-asset'"); err != nil {
		t.Fatal(err)
	}
	mutation, err := store.AddCollectionItem(ctx, assets.AddCollectionItemInput{
		CollectionID:   collection.ID,
		AssetID:        "management-default-asset",
		RemoteItemID:   "management-default-item",
		DisplayName:    "Managed default",
		SourceRevision: "source",
		CallerService:  "helper",
		IdempotencyKey: "item-management-defaults",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	item := mutation.Item
	if item.RetentionExempt || item.UpdatedRevision != item.CreatedRevision || !item.UpdatedAt.Equal(item.CreatedAt) {
		t.Fatalf("unexpected item defaults: %+v", item)
	}
}

func TestCollectionManagementConstraints(t *testing.T) {
	db := integrationDB(t)
	now := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	insertCollection(t, db, "collection", now)

	for _, retentionDays := range []int{0, 366} {
		if _, err := db.Exec(`INSERT INTO asset_collections(id,namespace,name,retention_days,created_by_service,created_at,updated_at) VALUES($1,'namespace','Managed',$2,'helper',$3,$3)`, fmt.Sprintf("invalid-retention-%d", retentionDays), retentionDays, now); err == nil {
			t.Fatalf("retention days %d was accepted", retentionDays)
		}
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES('ticket-item','collection','ticket-item','Ticket','source',1,false,1,$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO asset_content_tickets(token_hash,collection_id,collection_item_id,asset_etag,user_id,roles,access_mode,expires_at,created_at) VALUES($1,'collection','ticket-item','etag','user',ARRAY[]::text[],'other',$2,$3)`, strings.Repeat("a", 64), now.Add(time.Minute), now); err == nil {
		t.Fatal("ticket access mode other was accepted")
	}
}

func TestManagedCollectionItemsAndCollectionRetention(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 19, 1, 0, 0, 0, time.UTC)
	insertCollection(t, db, "managed-items", now)
	for _, item := range []struct {
		id, name  string
		createdAt time.Time
	}{
		{id: "same-a", name: "Sunday.mp4", createdAt: now},
		{id: "same-b", name: "Sunday.mp4", createdAt: now},
		{id: "older", name: "Weekday.mp4", createdAt: now.Add(-time.Minute)},
	} {
		if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES($1,'managed-items',$1,$2,'source',1,false,1,$3,$3)`, item.id, item.name, item.createdAt); err != nil {
			t.Fatal(err)
		}
	}
	for index := range 98 {
		id := fmt.Sprintf("older-%03d", index)
		if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES($1,'managed-items',$1,$1,'source',1,false,1,$2,$2)`, id, now.Add(-2*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	page, err := store.ListManagedCollectionItems(ctx, "managed-items", "hhc-line-function-bot", "SUNday", "", 1)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "same-b" || !page.HasMore || page.Cursor == "" {
		t.Fatalf("first page=%+v err=%v", page, err)
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"assetId", "blob", "remoteItemId", "ownerService"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("managed response contains %q", forbidden)
		}
	}
	second, err := store.ListManagedCollectionItems(ctx, "managed-items", "hhc-line-function-bot", "sunday", page.Cursor, 1)
	if err != nil || len(second.Items) != 1 || second.Items[0].ID != "same-a" || second.HasMore {
		t.Fatalf("second page=%+v err=%v", second, err)
	}
	if _, err := store.ListManagedCollectionItems(ctx, "managed-items", "hhc-line-function-bot", "", "not-a-cursor", 100); !errors.Is(err, assets.ErrInvalidInput) {
		t.Fatalf("malformed cursor err=%v", err)
	}
	bounded, err := store.ListManagedCollectionItems(ctx, "managed-items", "hhc-line-function-bot", "", "", 100)
	if err != nil || len(bounded.Items) != 100 || !bounded.HasMore || bounded.Items[0].ID != "same-b" {
		t.Fatalf("bounded page=%+v err=%v", bounded, err)
	}
	if _, err := store.ListManagedCollectionItems(ctx, "managed-items", "other-service", "", "", 100); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("cross-owner list err=%v", err)
	}
	if _, err := db.Exec(`INSERT INTO asset_collections(id,namespace,name,created_by_service,created_at,updated_at) VALUES('managed-other-namespace','other','Other','hhc-line-function-bot',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListManagedCollectionItems(ctx, "managed-other-namespace", "hhc-line-function-bot", "", "", 100); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("cross-namespace list err=%v", err)
	}

	for _, retentionDays := range []int{1, 365} {
		updated, err := store.UpdateCollectionRetention(ctx, assets.UpdateCollectionRetentionInput{CollectionID: "managed-items", RetentionDays: retentionDays, CallerService: "hhc-line-function-bot", IdempotencyKey: fmt.Sprintf("retention-%d", retentionDays)}, now.Add(time.Duration(retentionDays)*time.Second))
		if err != nil || updated.RetentionDays != retentionDays || updated.Revision != 1 {
			t.Fatalf("retention days=%d updated=%+v err=%v", retentionDays, updated, err)
		}
	}
}

func TestCollectionMutationsReplayConflictAndRevision(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 16, 1, 0, 0, 0, time.UTC)
	insertAsset(t, db, "collection-mutation-asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	if _, err := db.Exec("UPDATE assets SET namespace='namespace',owner_service='helper' WHERE id='collection-mutation-asset'"); err != nil {
		t.Fatal(err)
	}

	create := assets.CreateCollectionInput{Namespace: "namespace", Name: "Media", CallerService: "helper", IdempotencyKey: "create"}
	collection, err := store.CreateCollection(ctx, create, now)
	if err != nil || collection.Revision != 1 || collection.CreatedByService != "helper" {
		t.Fatalf("create=%+v err=%v", collection, err)
	}
	replay, err := store.CreateCollection(ctx, create, now.Add(time.Second))
	if err != nil || replay.ID != collection.ID || replay.Revision != 1 || replay.CreatedByService != "helper" {
		t.Fatalf("create replay=%+v err=%v", replay, err)
	}
	conflict := create
	conflict.Name = "Other"
	if _, err := store.CreateCollection(ctx, conflict, now); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("create conflict err=%v", err)
	}

	renamed, err := store.RenameCollection(ctx, assets.RenameCollectionInput{CollectionID: collection.ID, Name: "Renamed", CallerService: "helper", IdempotencyKey: "rename"}, now)
	if err != nil || renamed.Revision != 2 || renamed.Name != "Renamed" {
		t.Fatalf("rename=%+v err=%v", renamed, err)
	}
	aclResult, err := store.AddCollectionACL(ctx, assets.AddCollectionACLInput{CollectionID: collection.ID, SubjectType: assets.SubjectUser, SubjectID: "user", Permission: assets.PermissionRead, CallerService: "helper", IdempotencyKey: "acl-add"}, now)
	if err != nil || aclResult.Collection.Revision != 3 || aclResult.ACL.ID == "" {
		t.Fatalf("add ACL=%+v err=%v", aclResult, err)
	}
	if _, err := store.AddCollectionACL(ctx, assets.AddCollectionACLInput{CollectionID: collection.ID, SubjectType: assets.SubjectUser, SubjectID: "user", Permission: assets.PermissionRead, CallerService: "helper", IdempotencyKey: "acl-duplicate"}, now); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("duplicate active ACL err=%v", err)
	}
	revoked, err := store.RevokeCollectionACL(ctx, assets.RevokeCollectionACLInput{CollectionID: collection.ID, ACLID: aclResult.ACL.ID, CallerService: "helper", IdempotencyKey: "acl-revoke"}, now)
	if err != nil || revoked.Collection.Revision != 4 || revoked.ACL.RevokedAt.IsZero() {
		t.Fatalf("revoke ACL=%+v err=%v", revoked, err)
	}
	itemResult, err := store.AddCollectionItem(ctx, assets.AddCollectionItemInput{CollectionID: collection.ID, AssetID: "collection-mutation-asset", RemoteItemID: "remote", DisplayName: "Media", SourceRevision: "source-1", CallerService: "helper", IdempotencyKey: "item-add"}, now)
	if err != nil || itemResult.Collection.Revision != 5 || itemResult.Item.CreatedRevision != 5 || itemResult.Item.ID == "" {
		t.Fatalf("add item=%+v err=%v", itemResult, err)
	}
	itemReplay, err := store.AddCollectionItem(ctx, assets.AddCollectionItemInput{CollectionID: collection.ID, AssetID: "collection-mutation-asset", RemoteItemID: "remote", DisplayName: "Media", SourceRevision: "source-1", CallerService: "helper", IdempotencyKey: "item-add"}, now.Add(time.Minute))
	if err != nil || itemReplay.Item.ID != itemResult.Item.ID || itemReplay.Collection.Revision != 5 {
		t.Fatalf("item replay=%+v err=%v", itemReplay, err)
	}
	if _, err := store.AddCollectionItem(ctx, assets.AddCollectionItemInput{CollectionID: collection.ID, AssetID: "collection-mutation-asset", RemoteItemID: "other", DisplayName: "Other", SourceRevision: "source-2", CallerService: "helper", IdempotencyKey: "item-duplicate"}, now); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("duplicate active asset err=%v", err)
	}
	deletedItem, err := store.DeleteCollectionItem(ctx, assets.DeleteCollectionItemInput{CollectionID: collection.ID, ItemID: itemResult.Item.ID, CallerService: "helper", IdempotencyKey: "item-delete"}, now)
	if err != nil || deletedItem.Collection.Revision != 6 || deletedItem.Tombstone.ID != itemResult.Item.ID || deletedItem.Tombstone.DeletedRevision != 6 {
		t.Fatalf("delete item=%+v err=%v", deletedItem, err)
	}
	if _, err := db.Exec(`INSERT INTO asset_content_tickets(token_hash,collection_id,collection_item_id,asset_etag,user_id,roles,expires_at,created_at) VALUES($1,$2,$3,'etag','user',ARRAY[]::text[],$4,$5)`, strings.Repeat("e", 64), collection.ID, itemResult.Item.ID, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	readded, err := store.AddCollectionItem(ctx, assets.AddCollectionItemInput{CollectionID: collection.ID, AssetID: "collection-mutation-asset", RemoteItemID: "remote", DisplayName: "Media again", SourceRevision: "source-2", CallerService: "helper", IdempotencyKey: "item-readd"}, now)
	if err != nil || readded.Collection.Revision != 7 || readded.Item.ID == itemResult.Item.ID {
		t.Fatalf("re-add item=%+v err=%v", readded, err)
	}
	var ticketItem string
	var ticketDeletedRevision int64
	if err := db.QueryRow(`SELECT i.id,i.deleted_revision FROM asset_content_tickets t JOIN asset_collection_items i ON i.id=t.collection_item_id AND i.collection_id=t.collection_id WHERE t.token_hash=$1`, strings.Repeat("e", 64)).Scan(&ticketItem, &ticketDeletedRevision); err != nil {
		t.Fatal(err)
	}
	if ticketItem != itemResult.Item.ID || ticketItem == readded.Item.ID || ticketDeletedRevision != 6 {
		t.Fatalf("ticketItem=%q newItem=%q deletedRevision=%d", ticketItem, readded.Item.ID, ticketDeletedRevision)
	}
	deleted, err := store.DeleteCollection(ctx, assets.DeleteCollectionInput{CollectionID: collection.ID, CallerService: "helper", IdempotencyKey: "collection-delete"}, now)
	if err != nil || deleted.Revision != 8 || deleted.DeletedAt.IsZero() {
		t.Fatalf("delete collection=%+v err=%v", deleted, err)
	}
	if replay, err := store.DeleteCollection(ctx, assets.DeleteCollectionInput{CollectionID: collection.ID, CallerService: "helper", IdempotencyKey: "collection-delete"}, now.Add(time.Minute)); err != nil || replay.Revision != 8 {
		t.Fatalf("delete replay=%+v err=%v", replay, err)
	}
}

func TestCollectionDeleteCascadesItemsAssetsTicketsAndReplays(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	createdAt := time.Date(2026, 8, 19, 6, 0, 0, 0, time.UTC)
	deletedAt := createdAt.Add(time.Minute)
	insertCollection(t, db, "collection-delete", createdAt)
	insertCollection(t, db, "collection-delete-shared", createdAt)
	for _, assetID := range []string{"collection-delete-owned", "collection-delete-referenced"} {
		insertAsset(t, db, assetID, assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, createdAt, time.Time{})
		if _, err := db.Exec(`UPDATE assets SET namespace='line.group.media-sync',owner_service='hhc-line-function-bot',owner_type='media_sync_ingest' WHERE id=$1`, assetID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`
		INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at)
		VALUES
		  ('collection-delete-active','collection-delete','collection-delete-owned','active','Active','source',1,false,1,$1,$1),
		  ('collection-delete-permanent','collection-delete','collection-delete-referenced','permanent','Permanent','source',1,true,1,$1,$1),
		  ('collection-delete-shared-item','collection-delete-shared','collection-delete-referenced','shared','Shared','source',1,false,1,$1,$1)`, createdAt); err != nil {
		t.Fatal(err)
	}
	ticketHash := strings.Repeat("c", 64)
	if _, err := db.Exec(`INSERT INTO asset_content_tickets(token_hash,collection_id,collection_item_id,asset_etag,user_id,roles,access_mode,expires_at,created_at) VALUES($1,'collection-delete','collection-delete-active','etag-collection-delete-owned','manager',ARRAY[]::text[],'manager',$2,$3)`, ticketHash, createdAt.Add(5*time.Minute), createdAt); err != nil {
		t.Fatal(err)
	}

	input := assets.DeleteCollectionInput{CollectionID: "collection-delete", CallerService: "hhc-line-function-bot", IdempotencyKey: "collection-delete"}
	deleted, err := store.DeleteCollection(ctx, input, deletedAt)
	if err != nil || deleted.Revision != 2 || !deleted.DeletedAt.Equal(deletedAt) {
		t.Fatalf("deleted=%+v err=%v", deleted, err)
	}
	replay, err := store.DeleteCollection(ctx, input, deletedAt.Add(time.Minute))
	if err != nil || replay.Revision != deleted.Revision || !replay.DeletedAt.Equal(deletedAt) {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	for _, itemID := range []string{"collection-delete-active", "collection-delete-permanent"} {
		var deletedRevision sql.NullInt64
		var itemDeletedAt sql.NullTime
		if err := db.QueryRow(`SELECT deleted_revision,deleted_at FROM asset_collection_items WHERE id=$1`, itemID).Scan(&deletedRevision, &itemDeletedAt); err != nil {
			t.Fatal(err)
		}
		if !deletedRevision.Valid || deletedRevision.Int64 != 2 || !itemDeletedAt.Valid || !itemDeletedAt.Time.Equal(deletedAt) {
			t.Fatalf("item=%s revision=%v deletedAt=%v", itemID, deletedRevision, itemDeletedAt)
		}
	}
	for assetID, wantDeleted := range map[string]bool{
		"collection-delete-owned":      true,
		"collection-delete-referenced": false,
	} {
		var assetDeletedAt sql.NullTime
		if err := db.QueryRow(`SELECT deleted_at FROM assets WHERE id=$1`, assetID).Scan(&assetDeletedAt); err != nil {
			t.Fatal(err)
		}
		if assetDeletedAt.Valid != wantDeleted || (wantDeleted && !assetDeletedAt.Time.Equal(deletedAt)) {
			t.Fatalf("asset=%s deletedAt=%v", assetID, assetDeletedAt)
		}
	}
	if _, err := store.RedeemContentTicket(ctx, ticketHash, deletedAt); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("ticket remained valid: %v", err)
	}
}

func TestAddCollectionItemFirstThenDeleteCollectionCascadesTheItem(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 19, 6, 30, 0, 0, time.UTC)
	insertCollection(t, db, "add-first-delete-second", now)
	insertAsset(t, db, "add-first-delete-second-asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	if _, err := db.Exec(`UPDATE assets SET namespace='line.group.media-sync',owner_service='hhc-line-function-bot',owner_type='media_sync_ingest' WHERE id='add-first-delete-second-asset'`); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.Exec(`SELECT id FROM asset_collections WHERE id='add-first-delete-second' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	added := make(chan error, 1)
	go func() {
		_, err := store.AddCollectionItem(context.Background(), assets.AddCollectionItemInput{
			CollectionID: "add-first-delete-second", AssetID: "add-first-delete-second-asset", RemoteItemID: "remote",
			DisplayName: "Media", SourceRevision: "source", CallerService: "hhc-line-function-bot", IdempotencyKey: "add-first",
		}, now)
		added <- err
	}()
	select {
	case err := <-added:
		t.Fatalf("add did not wait for collection lock: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	deleted := make(chan error, 1)
	go func() {
		_, err := store.DeleteCollection(context.Background(), assets.DeleteCollectionInput{
			CollectionID: "add-first-delete-second", CallerService: "hhc-line-function-bot", IdempotencyKey: "delete-second",
		}, now.Add(time.Minute))
		deleted <- err
	}()
	select {
	case err := <-deleted:
		t.Fatalf("delete did not wait for collection lock: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-added; err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("delete: %v", err)
	}
	var revision, createdRevision int64
	var deletedRevision sql.NullInt64
	var assetDeletedAt sql.NullTime
	if err := db.QueryRow(`SELECT c.revision,i.created_revision,i.deleted_revision,a.deleted_at FROM asset_collections c JOIN asset_collection_items i ON i.collection_id=c.id JOIN assets a ON a.id=i.asset_id WHERE c.id='add-first-delete-second'`).Scan(&revision, &createdRevision, &deletedRevision, &assetDeletedAt); err != nil {
		t.Fatal(err)
	}
	if revision != 3 || createdRevision != 2 || !deletedRevision.Valid || deletedRevision.Int64 != 3 || !assetDeletedAt.Valid {
		t.Fatalf("revision=%d created=%d deleted=%v assetDeleted=%v", revision, createdRevision, deletedRevision, assetDeletedAt.Valid)
	}
}

func TestAddCollectionItemWaitsForDeleteCollectionAndCannotResurrectIt(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 19, 7, 0, 0, 0, time.UTC)
	insertCollection(t, db, "delete-first-add-second", now)
	for _, assetID := range []string{"delete-first-existing-asset", "delete-first-new-asset"} {
		insertAsset(t, db, assetID, assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
		if _, err := db.Exec(`UPDATE assets SET namespace='line.group.media-sync',owner_service='hhc-line-function-bot',owner_type='media_sync_ingest' WHERE id=$1`, assetID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES('delete-first-existing-item','delete-first-add-second','delete-first-existing-asset','existing','Existing','source',1,false,1,$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.Exec(`SELECT id FROM asset_collections WHERE id='delete-first-add-second' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() {
		_, err := store.DeleteCollection(context.Background(), assets.DeleteCollectionInput{
			CollectionID: "delete-first-add-second", CallerService: "hhc-line-function-bot", IdempotencyKey: "delete-first",
		}, now.Add(time.Minute))
		deleted <- err
	}()
	select {
	case err := <-deleted:
		t.Fatalf("delete did not wait for collection lock: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := blocker.Exec(`SELECT id FROM assets WHERE id='delete-first-existing-asset' FOR UPDATE`); err != nil {
		blocker.Rollback()
		t.Fatalf("delete locked asset before collection: %v", err)
	}
	added := make(chan error, 1)
	go func() {
		_, err := store.AddCollectionItem(context.Background(), assets.AddCollectionItemInput{
			CollectionID: "delete-first-add-second", AssetID: "delete-first-new-asset", RemoteItemID: "new",
			DisplayName: "New", SourceRevision: "source", CallerService: "hhc-line-function-bot", IdempotencyKey: "add-second",
		}, now.Add(2*time.Minute))
		added <- err
	}()
	select {
	case err := <-added:
		t.Fatalf("add did not wait for collection lock: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := <-added; !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("add after delete: %v", err)
	}
	var revision int64
	var deletedRevision sql.NullInt64
	var existingDeletedAt, newDeletedAt sql.NullTime
	if err := db.QueryRow(`SELECT c.revision,i.deleted_revision,existing.deleted_at,new_asset.deleted_at FROM asset_collections c JOIN asset_collection_items i ON i.collection_id=c.id JOIN assets existing ON existing.id=i.asset_id CROSS JOIN assets new_asset WHERE c.id='delete-first-add-second' AND new_asset.id='delete-first-new-asset'`).Scan(&revision, &deletedRevision, &existingDeletedAt, &newDeletedAt); err != nil {
		t.Fatal(err)
	}
	if revision != 2 || !deletedRevision.Valid || deletedRevision.Int64 != 2 || !existingDeletedAt.Valid || newDeletedAt.Valid {
		t.Fatalf("revision=%d deleted=%v existingAsset=%v newAsset=%v", revision, deletedRevision, existingDeletedAt.Valid, newDeletedAt.Valid)
	}
}

func TestRenameCollectionItemUpdatesRevisionWithoutChangingContentIdentity(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 19, 2, 0, 0, 0, time.UTC)
	insertAsset(t, db, "rename-item-asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	if _, err := db.Exec("UPDATE assets SET namespace='namespace',owner_service='helper' WHERE id='rename-item-asset'"); err != nil {
		t.Fatal(err)
	}
	collection, err := store.CreateCollection(ctx, assets.CreateCollectionInput{Namespace: "namespace", Name: "Media", CallerService: "helper", IdempotencyKey: "rename-create"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddCollectionACL(ctx, assets.AddCollectionACLInput{CollectionID: collection.ID, SubjectType: assets.SubjectUser, SubjectID: "user", Permission: assets.PermissionRead, CallerService: "helper", IdempotencyKey: "rename-acl"}, now); err != nil {
		t.Fatal(err)
	}
	added, err := store.AddCollectionItem(ctx, assets.AddCollectionItemInput{CollectionID: collection.ID, AssetID: "rename-item-asset", RemoteItemID: "remote", DisplayName: "Original.MP4", SourceRevision: "source-1", CallerService: "helper", IdempotencyKey: "rename-add"}, now)
	if err != nil {
		t.Fatal(err)
	}
	subject := assets.CollectionSubject{UserID: "user", Roles: []string{assets.CollectionReaderRole}}
	before, err := store.GetAuthorizedCollectionItem(ctx, collection.ID, added.Item.ID, subject)
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := store.RenameCollectionItem(ctx, assets.RenameCollectionItemInput{CollectionID: collection.ID, ItemID: added.Item.ID, DisplayName: "  Renamed.mp4  ", CallerService: "helper", IdempotencyKey: "rename-item"}, now.Add(time.Minute))
	if err != nil || renamed.DisplayName != "Renamed.mp4" {
		t.Fatalf("rename=%+v err=%v", renamed, err)
	}
	after, err := store.GetAuthorizedCollectionItem(ctx, collection.ID, added.Item.ID, subject)
	if err != nil {
		t.Fatal(err)
	}
	if after.UpdatedRevision != added.Collection.Revision+1 || after.DisplayName != "Renamed.mp4" || before.ETag != after.ETag || before.SizeBytes != after.SizeBytes || before.MIMEType != after.MIMEType || before.SourceRevision != after.SourceRevision || !before.CreatedAt.Equal(after.CreatedAt) {
		t.Fatalf("before=%+v after=%+v", before, after)
	}
	changes, err := store.CollectionChanges(ctx, collection.ID, encodeChangeCursor(changeCursor{Mode: changeModeDelta, CollectionID: collection.ID, FromRevision: added.Collection.Revision, ToRevision: after.UpdatedRevision}), subject)
	if err != nil || len(changes.Items) != 1 || len(changes.Tombstones) != 0 || changes.Items[0].ID != added.Item.ID || changes.Items[0].DisplayName != "Renamed.mp4" || changes.Items[0].CreatedRevision != added.Item.CreatedRevision || changes.Items[0].UpdatedRevision != after.UpdatedRevision {
		t.Fatalf("changes=%+v err=%v", changes, err)
	}
	if _, err := store.RenameCollectionItem(ctx, assets.RenameCollectionItemInput{CollectionID: collection.ID, ItemID: added.Item.ID, DisplayName: "Renamed.mp4", CallerService: "helper", IdempotencyKey: "rename-same"}, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetAuthorizedCollection(ctx, collection.ID, subject)
	if err != nil || current.Revision != after.UpdatedRevision {
		t.Fatalf("collection=%+v err=%v", current, err)
	}
	if _, err := store.RenameCollectionItem(ctx, assets.RenameCollectionItemInput{CollectionID: collection.ID, ItemID: added.Item.ID, DisplayName: "Renamed.mp4", CallerService: "helper", IdempotencyKey: "rename-same"}, now.Add(3*time.Minute)); err != nil {
		t.Fatalf("same-name replay err=%v", err)
	}
	if _, err := store.RenameCollectionItem(ctx, assets.RenameCollectionItemInput{CollectionID: collection.ID, ItemID: "missing", DisplayName: "Missing.mp4", CallerService: "helper", IdempotencyKey: "rename-missing"}, now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("missing err=%v", err)
	}
	for _, displayName := range []string{"Renamed.avi", "bad/name.mp4", `bad\\name.mp4`, "bad\x00.mp4", strings.Repeat("a", 256)} {
		if _, err := store.RenameCollectionItem(ctx, assets.RenameCollectionItemInput{CollectionID: collection.ID, ItemID: added.Item.ID, DisplayName: displayName, CallerService: "helper", IdempotencyKey: "rename-invalid-" + strings.ReplaceAll(displayName, "/", "-")}, now); !errors.Is(err, assets.ErrInvalidInput) {
			t.Fatalf("displayName=%q err=%v", displayName, err)
		}
	}
	insertAsset(t, db, "rename-item-duplicate", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	if _, err := db.Exec("UPDATE assets SET namespace='namespace',owner_service='helper' WHERE id='rename-item-duplicate'"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddCollectionItem(ctx, assets.AddCollectionItemInput{CollectionID: collection.ID, AssetID: "rename-item-duplicate", RemoteItemID: "duplicate", DisplayName: "Renamed.mp4", SourceRevision: "source-2", CallerService: "helper", IdempotencyKey: "rename-duplicate"}, now); err != nil {
		t.Fatalf("duplicate display name err=%v", err)
	}
	deleted, err := store.DeleteCollectionItem(ctx, assets.DeleteCollectionItemInput{CollectionID: collection.ID, ItemID: added.Item.ID, CallerService: "helper", IdempotencyKey: "rename-delete"}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RenameCollectionItem(ctx, assets.RenameCollectionItemInput{CollectionID: collection.ID, ItemID: added.Item.ID, DisplayName: "Deleted.mp4", CallerService: "helper", IdempotencyKey: "rename-deleted"}, now); !errors.Is(err, assets.ErrNotFound) || deleted.Item.DeletedRevision == 0 {
		t.Fatalf("deleted=%+v err=%v", deleted, err)
	}
}

func TestCollectionMutationsRejectNonOwner(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 16, 2, 0, 0, 0, time.UTC)
	insertAsset(t, db, "owner-asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	if _, err := db.Exec("UPDATE assets SET namespace='namespace',owner_service='owner' WHERE id='owner-asset'"); err != nil {
		t.Fatal(err)
	}
	collection, err := store.CreateCollection(ctx, assets.CreateCollectionInput{Namespace: "namespace", Name: "Owned", CallerService: "owner", IdempotencyKey: "create-owned"}, now)
	if err != nil {
		t.Fatal(err)
	}
	acl, err := store.AddCollectionACL(ctx, assets.AddCollectionACLInput{CollectionID: collection.ID, SubjectType: assets.SubjectUser, SubjectID: "user", Permission: assets.PermissionRead, CallerService: "owner", IdempotencyKey: "owner-acl"}, now)
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.AddCollectionItem(ctx, assets.AddCollectionItemInput{CollectionID: collection.ID, AssetID: "owner-asset", RemoteItemID: "remote", DisplayName: "Media", SourceRevision: "source", CallerService: "owner", IdempotencyKey: "owner-item"}, now)
	if err != nil {
		t.Fatal(err)
	}

	checks := []func() error{
		func() error {
			_, err := store.RenameCollection(ctx, assets.RenameCollectionInput{CollectionID: collection.ID, Name: "No", CallerService: "other", IdempotencyKey: "wrong-rename"}, now)
			return err
		},
		func() error {
			_, err := store.DeleteCollection(ctx, assets.DeleteCollectionInput{CollectionID: collection.ID, CallerService: "other", IdempotencyKey: "wrong-delete"}, now)
			return err
		},
		func() error {
			_, err := store.AddCollectionACL(ctx, assets.AddCollectionACLInput{CollectionID: collection.ID, SubjectType: assets.SubjectRole, SubjectID: "role", Permission: assets.PermissionRead, CallerService: "other", IdempotencyKey: "wrong-add-acl"}, now)
			return err
		},
		func() error {
			_, err := store.RevokeCollectionACL(ctx, assets.RevokeCollectionACLInput{CollectionID: collection.ID, ACLID: acl.ACL.ID, CallerService: "other", IdempotencyKey: "wrong-revoke-acl"}, now)
			return err
		},
		func() error {
			_, err := store.AddCollectionItem(ctx, assets.AddCollectionItemInput{CollectionID: collection.ID, AssetID: "owner-asset", RemoteItemID: "other", DisplayName: "No", SourceRevision: "source", CallerService: "other", IdempotencyKey: "wrong-add-item"}, now)
			return err
		},
		func() error {
			_, err := store.DeleteCollectionItem(ctx, assets.DeleteCollectionItemInput{CollectionID: collection.ID, ItemID: item.Item.ID, CallerService: "other", IdempotencyKey: "wrong-delete-item"}, now)
			return err
		},
	}
	for index, check := range checks {
		if err := check(); !errors.Is(err, assets.ErrForbidden) {
			t.Fatalf("check %d err=%v", index, err)
		}
	}
}

func TestAddCollectionItemRejectsInvalidAssetMembershipStates(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 16, 2, 30, 0, 0, time.UTC)
	insertCollection(t, db, "media-state-collection", now)
	tests := []struct {
		name                     string
		upload, scan, processing string
		namespace, owner         string
		deletedAt, purgedAt      time.Time
		want                     error
	}{
		{name: "upload incomplete", upload: string(assets.UploadCreated), scan: string(assets.ScanClean), processing: string(assets.ProcessingReady), want: assets.ErrConflict},
		{name: "scan pending", upload: string(assets.UploadCompleted), scan: string(assets.ScanPending), processing: string(assets.ProcessingReady), want: assets.ErrConflict},
		{name: "scan infected", upload: string(assets.UploadCompleted), scan: string(assets.ScanInfected), processing: string(assets.ProcessingReady), want: assets.ErrConflict},
		{name: "scan failed", upload: string(assets.UploadCompleted), scan: string(assets.ScanFailed), processing: string(assets.ProcessingReady), want: assets.ErrConflict},
		{name: "processing pending", upload: string(assets.UploadCompleted), scan: string(assets.ScanClean), processing: string(assets.ProcessingPending), want: assets.ErrConflict},
		{name: "processing failed", upload: string(assets.UploadCompleted), scan: string(assets.ScanClean), processing: string(assets.ProcessingFailed), want: assets.ErrConflict},
		{name: "namespace mismatch", upload: string(assets.UploadCompleted), scan: string(assets.ScanClean), processing: string(assets.ProcessingReady), namespace: "other", want: assets.ErrConflict},
		{name: "owner mismatch", upload: string(assets.UploadCompleted), scan: string(assets.ScanClean), processing: string(assets.ProcessingReady), owner: "other", want: assets.ErrForbidden},
		{name: "deleted", upload: string(assets.UploadCompleted), scan: string(assets.ScanClean), processing: string(assets.ProcessingReady), deletedAt: now, want: assets.ErrNotFound},
		{name: "purged", upload: string(assets.UploadCompleted), scan: string(assets.ScanClean), processing: string(assets.ProcessingReady), purgedAt: now, want: assets.ErrNotFound},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			id := fmt.Sprintf("membership-state-%d", index)
			namespace := test.namespace
			if namespace == "" {
				namespace = "line.group.media-sync"
			}
			owner := test.owner
			if owner == "" {
				owner = "hhc-line-function-bot"
			}
			query := "INSERT INTO assets(id,namespace,owner_service,owner_type,owner_id,object_key,expected_mime_type,upload_status,scan_status,processing_status,visibility,created_at,updated_at,deleted_at,purged_at,detected_mime_type,size_bytes,etag,scan_event_id) VALUES($1,$2,$3,'line_group','group',$4,'video/mp4',$5,$6,$7,'restricted',$8,$8,NULLIF($9,'0001-01-01'::timestamptz),NULLIF($10,'0001-01-01'::timestamptz),'video/mp4',20,'etag','event')"
			if _, err := db.Exec(query, id, namespace, owner, "assets/"+id, test.upload, test.scan, test.processing, now, test.deletedAt, test.purgedAt); err != nil {
				t.Fatal(err)
			}
			_, err := store.AddCollectionItem(ctx, assets.AddCollectionItemInput{
				CollectionID: "media-state-collection", AssetID: id, RemoteItemID: id,
				DisplayName: "Media", SourceRevision: "source", CallerService: "hhc-line-function-bot",
				IdempotencyKey: "membership-" + id,
			}, now)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestAddCollectionItemRechecksAssetStateAfterWaitingForAssetLock(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	now := time.Date(2026, 8, 16, 2, 45, 0, 0, time.UTC)
	insertCollection(t, db, "media-race-collection", now)
	insertAsset(t, db, "media-race-asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	if _, err := db.Exec("UPDATE assets SET namespace='line.group.media-sync',owner_service='hhc-line-function-bot' WHERE id='media-race-asset'"); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("UPDATE assets SET scan_status='pending' WHERE id='media-race-asset'"); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := store.AddCollectionItem(context.Background(), assets.AddCollectionItemInput{
			CollectionID: "media-race-collection", AssetID: "media-race-asset", RemoteItemID: "race",
			DisplayName: "Race", SourceRevision: "source", CallerService: "hhc-line-function-bot",
			IdempotencyKey: "membership-race",
		}, now)
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("add did not wait for asset lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("error after state transition = %v", err)
	}
}

func TestConcurrentCreateCollectionIdempotency(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprintf("conflict=%v", conflict), func(t *testing.T) {
			db := integrationDB(t)
			store := New(db)
			now := time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC)
			start := make(chan struct{})
			results := make(chan struct {
				collection assets.Collection
				err        error
			}, 2)
			for index := range 2 {
				go func() {
					input := assets.CreateCollectionInput{Namespace: "namespace", Name: "Media", CallerService: "helper", IdempotencyKey: "same-key"}
					if conflict && index == 1 {
						input.Name = "Other"
					}
					<-start
					collection, err := store.CreateCollection(context.Background(), input, now)
					results <- struct {
						collection assets.Collection
						err        error
					}{collection, err}
				}()
			}
			close(start)
			var values []assets.Collection
			var conflicts int
			for range 2 {
				result := <-results
				if errors.Is(result.err, assets.ErrConflict) {
					conflicts++
				} else if result.err != nil {
					t.Fatal(result.err)
				} else {
					values = append(values, result.collection)
				}
			}
			wantConflicts := 0
			if conflict {
				wantConflicts = 1
			}
			if conflicts != wantConflicts || len(values) != 2-wantConflicts || (!conflict && values[0].ID != values[1].ID) {
				t.Fatalf("values=%+v conflicts=%d", values, conflicts)
			}
			var collections, mutations, responses int
			if err := db.QueryRow(`SELECT count(*) FROM asset_collections`).Scan(&collections); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT count(*),count(response_json) FROM asset_collection_mutations`).Scan(&mutations, &responses); err != nil {
				t.Fatal(err)
			}
			if collections != 1 || mutations != 1 || responses != 1 {
				t.Fatalf("collections=%d mutations=%d responses=%d", collections, mutations, responses)
			}
		})
	}
}

func TestConcurrentCollectionMutationsIncrementEveryRevision(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	now := time.Date(2026, 8, 16, 3, 30, 0, 0, time.UTC)
	collection, err := store.CreateCollection(context.Background(), assets.CreateCollectionInput{Namespace: "namespace", Name: "Concurrent", CallerService: "helper", IdempotencyKey: "concurrent-create"}, now)
	if err != nil {
		t.Fatal(err)
	}
	const mutations = 16
	start := make(chan struct{})
	errorsCh := make(chan error, mutations)
	for index := range mutations {
		go func() {
			<-start
			_, err := store.AddCollectionACL(context.Background(), assets.AddCollectionACLInput{CollectionID: collection.ID, SubjectType: assets.SubjectUser, SubjectID: fmt.Sprintf("user-%d", index), Permission: assets.PermissionRead, CallerService: "helper", IdempotencyKey: fmt.Sprintf("concurrent-%d", index)}, now)
			errorsCh <- err
		}()
	}
	close(start)
	for range mutations {
		if err := <-errorsCh; err != nil {
			t.Fatal(err)
		}
	}
	var revision int64
	if err := db.QueryRow(`SELECT revision FROM asset_collections WHERE id=$1`, collection.ID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if revision != mutations+1 {
		t.Fatalf("revision=%d", revision)
	}
}

func TestCollectionAssetDeleteTombstonesMembershipsAndRacesAdd(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 16, 4, 0, 0, 0, time.UTC)
	insertAsset(t, db, "delete-asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	if _, err := db.Exec("UPDATE assets SET namespace='namespace',owner_service='helper' WHERE id='delete-asset'"); err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateCollection(ctx, assets.CreateCollectionInput{Namespace: "namespace", Name: "First", CallerService: "helper", IdempotencyKey: "first"}, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateCollection(ctx, assets.CreateCollectionInput{Namespace: "namespace", Name: "Second", CallerService: "helper", IdempotencyKey: "second"}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, collection := range []assets.Collection{first, second} {
		if _, err := store.AddCollectionItem(ctx, assets.AddCollectionItemInput{CollectionID: collection.ID, AssetID: "delete-asset", RemoteItemID: "remote-" + collection.ID, DisplayName: "Media", SourceRevision: "source", CallerService: "helper", IdempotencyKey: "add-" + collection.ID}, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SoftDeleteAsset(ctx, "delete-asset", "helper", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.SoftDeleteAsset(ctx, "delete-asset", "helper", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, collection := range []assets.Collection{first, second} {
		var revision, deletedRevision int64
		if err := db.QueryRow(`SELECT c.revision,i.deleted_revision FROM asset_collections c JOIN asset_collection_items i ON i.collection_id=c.id WHERE c.id=$1`, collection.ID).Scan(&revision, &deletedRevision); err != nil {
			t.Fatal(err)
		}
		if revision != 3 || deletedRevision != 3 {
			t.Fatalf("collection=%s revision=%d deletedRevision=%d", collection.ID, revision, deletedRevision)
		}
	}

	blobs := &recordingLifecycleBlobStore{}
	processed, err := lifecycle.NewWorker(store, blobs).ProcessOne(ctx)
	if err != nil || !processed {
		t.Fatalf("purge worker processed=%v err=%v", processed, err)
	}
	if !blobs.deleted["assets/delete-asset"] {
		t.Fatalf("deleted blobs=%v", blobs.deleted)
	}
	var purgedAt sql.NullTime
	if err := db.QueryRow(`SELECT purged_at FROM assets WHERE id='delete-asset'`).Scan(&purgedAt); err != nil {
		t.Fatal(err)
	}
	if !purgedAt.Valid {
		t.Fatal("purged_at was not written")
	}
	if _, err := db.Exec(`UPDATE assets SET purged_at=$2 WHERE id=$1`, "delete-asset", now.Add(-181*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeleteExpiredPurge(ctx, now.Add(-180*24*time.Hour), 10)
	if err != nil || deleted != 1 {
		t.Fatalf("retention deleted=%d err=%v", deleted, err)
	}
	var preserved, nullAssets int
	if err := db.QueryRow(`SELECT count(*),count(*) FILTER (WHERE asset_id IS NULL) FROM asset_collection_items`).Scan(&preserved, &nullAssets); err != nil {
		t.Fatal(err)
	}
	if preserved != 2 || nullAssets != 2 {
		t.Fatalf("preserved=%d nullAssets=%d", preserved, nullAssets)
	}
	for _, collection := range []assets.Collection{first, second} {
		var revision, deletedRevision int64
		var assetID sql.NullString
		if err := db.QueryRow(`SELECT c.revision,i.deleted_revision,i.asset_id FROM asset_collections c JOIN asset_collection_items i ON i.collection_id=c.id WHERE c.id=$1`, collection.ID).Scan(&revision, &deletedRevision, &assetID); err != nil {
			t.Fatal(err)
		}
		if revision != 3 || deletedRevision != 3 || assetID.Valid {
			t.Fatalf("retained collection=%s revision=%d deletedRevision=%d assetID=%v", collection.ID, revision, deletedRevision, assetID)
		}
	}

	insertAsset(t, db, "race-asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	if _, err := db.Exec("UPDATE assets SET namespace='namespace',owner_service='helper' WHERE id='race-asset'"); err != nil {
		t.Fatal(err)
	}
	raceCollection, err := store.CreateCollection(ctx, assets.CreateCollectionInput{Namespace: "namespace", Name: "Race", CallerService: "helper", IdempotencyKey: "race"}, now)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errorsCh := make(chan error, 2)
	go func() {
		<-start
		_, err := store.AddCollectionItem(context.Background(), assets.AddCollectionItemInput{CollectionID: raceCollection.ID, AssetID: "race-asset", RemoteItemID: "race-remote", DisplayName: "Race", SourceRevision: "source", CallerService: "helper", IdempotencyKey: "race-add"}, now)
		errorsCh <- err
	}()
	go func() {
		<-start
		errorsCh <- store.SoftDeleteAsset(context.Background(), "race-asset", "helper", now)
	}()
	close(start)
	for range 2 {
		err := <-errorsCh
		if err != nil && !errors.Is(err, assets.ErrNotFound) {
			t.Fatal(err)
		}
	}
	var active int
	if err := db.QueryRow(`SELECT count(*) FROM asset_collection_items WHERE collection_id=$1 AND deleted_revision IS NULL`, raceCollection.ID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("active memberships after asset delete=%d", active)
	}
}

func TestCollectionReaderAndManagedAuthorizationMatrix(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 16, 5, 0, 0, 0, time.UTC)
	var owned []assets.Collection
	for index := range 3 {
		collection, err := store.CreateCollection(ctx, assets.CreateCollectionInput{Namespace: "namespace", Name: fmt.Sprintf("Owned %d", index), CallerService: "helper", IdempotencyKey: fmt.Sprintf("owned-%d", index)}, now)
		if err != nil {
			t.Fatal(err)
		}
		owned = append(owned, collection)
		if _, err := store.AddCollectionACL(ctx, assets.AddCollectionACLInput{CollectionID: collection.ID, SubjectType: assets.SubjectUser, SubjectID: "user", Permission: assets.PermissionRead, CallerService: "helper", IdempotencyKey: fmt.Sprintf("user-acl-%d", index)}, now); err != nil {
			t.Fatal(err)
		}
	}
	other, err := store.CreateCollection(ctx, assets.CreateCollectionInput{Namespace: "namespace", Name: "Other", CallerService: "other-helper", IdempotencyKey: "other"}, now)
	if err != nil {
		t.Fatal(err)
	}
	roleACL, err := store.AddCollectionACL(ctx, assets.AddCollectionACLInput{CollectionID: other.ID, SubjectType: assets.SubjectRole, SubjectID: "team", Permission: assets.PermissionRead, CallerService: "other-helper", IdempotencyKey: "role-acl"}, now)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.ListAuthorizedCollections(ctx, assets.CollectionSubject{UserID: "user", Roles: []string{"admin"}}, "", 10); !errors.Is(err, assets.ErrForbidden) {
		t.Fatalf("missing global role err=%v", err)
	}
	empty, err := store.ListAuthorizedCollections(ctx, assets.CollectionSubject{UserID: "no-acl", Roles: []string{assets.CollectionReaderRole}}, "", 10)
	if err != nil || len(empty.Collections) != 0 {
		t.Fatalf("no ACL page=%+v err=%v", empty, err)
	}
	firstPage, err := store.ListAuthorizedCollections(ctx, assets.CollectionSubject{UserID: "user", Roles: []string{assets.CollectionReaderRole}}, "", 2)
	if err != nil || len(firstPage.Collections) != 2 || !firstPage.HasMore || firstPage.Cursor == "" {
		t.Fatalf("first reader page=%+v err=%v", firstPage, err)
	}
	secondPage, err := store.ListAuthorizedCollections(ctx, assets.CollectionSubject{UserID: "user", Roles: []string{assets.CollectionReaderRole}}, firstPage.Cursor, 2)
	if err != nil || len(secondPage.Collections) != 1 || secondPage.HasMore {
		t.Fatalf("second reader page=%+v err=%v", secondPage, err)
	}
	seen := map[string]bool{}
	for _, collection := range append(firstPage.Collections, secondPage.Collections...) {
		if seen[collection.ID] {
			t.Fatalf("duplicate reader collection %s", collection.ID)
		}
		seen[collection.ID] = true
	}
	rolePage, err := store.ListAuthorizedCollections(ctx, assets.CollectionSubject{UserID: "role-user", Roles: []string{assets.CollectionReaderRole, "team"}}, "", 10)
	if err != nil || len(rolePage.Collections) != 1 || rolePage.Collections[0].ID != other.ID {
		t.Fatalf("role page=%+v err=%v", rolePage, err)
	}
	if _, err := store.GetAuthorizedCollection(ctx, owned[0].ID, assets.CollectionSubject{UserID: "no-acl", Roles: []string{assets.CollectionReaderRole}}); !errors.Is(err, assets.ErrForbidden) {
		t.Fatalf("direct unauthorized err=%v", err)
	}
	if _, err := store.GetAuthorizedCollection(ctx, "missing", assets.CollectionSubject{UserID: "user", Roles: []string{assets.CollectionReaderRole}}); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("direct missing err=%v", err)
	}
	if _, err := store.RevokeCollectionACL(ctx, assets.RevokeCollectionACLInput{CollectionID: other.ID, ACLID: roleACL.ACL.ID, CallerService: "other-helper", IdempotencyKey: "revoke-role"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetAuthorizedCollection(ctx, other.ID, assets.CollectionSubject{UserID: "role-user", Roles: []string{assets.CollectionReaderRole, "team"}}); !errors.Is(err, assets.ErrForbidden) {
		t.Fatalf("revoked role ACL err=%v", err)
	}
	if _, err := store.DeleteCollection(ctx, assets.DeleteCollectionInput{CollectionID: owned[0].ID, CallerService: "helper", IdempotencyKey: "delete-reader"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetAuthorizedCollection(ctx, owned[0].ID, assets.CollectionSubject{UserID: "user", Roles: []string{assets.CollectionReaderRole}}); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("deleted collection err=%v", err)
	}

	managedFirst, err := store.ListManagedCollections(ctx, "helper", "", 1)
	if err != nil || len(managedFirst.Collections) != 1 || !managedFirst.HasMore || len(managedFirst.Collections[0].ACLs) == 0 {
		t.Fatalf("managed first=%+v err=%v", managedFirst, err)
	}
	managedSecond, err := store.ListManagedCollections(ctx, "helper", managedFirst.Cursor, 10)
	if err != nil || len(managedSecond.Collections) != 1 {
		t.Fatalf("managed second=%+v err=%v", managedSecond, err)
	}
	if _, err := store.GetManagedCollection(ctx, other.ID, "helper"); !errors.Is(err, assets.ErrForbidden) {
		t.Fatalf("managed wrong owner err=%v", err)
	}
	managed, err := store.GetManagedCollection(ctx, owned[1].ID, "helper")
	if err != nil || managed.Collection.ID != owned[1].ID || len(managed.ACLs) != 1 {
		t.Fatalf("managed=%+v err=%v", managed, err)
	}
}

func TestCollectionReaderGetAuthorizedItemRechecksLiveAuthorizationAndOccurrence(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 16, 5, 30, 0, 0, time.UTC)
	insertAuthorizedCollection(t, db, "reader-item-collection", 2, "user", now)
	insertAuthorizedCollection(t, db, "other-reader-collection", 2, "other", now)
	insertAsset(t, db, "reader-item-asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES('reader-item','reader-item-collection','reader-item-asset','remote-reader','Reader','source',2,false,2,$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_acl(id,collection_id,subject_type,subject_id,permission,created_at) VALUES('reader-role-acl','reader-item-collection','role','team','read',$1)`, now); err != nil {
		t.Fatal(err)
	}

	for _, subject := range []assets.CollectionSubject{
		{UserID: "user", Roles: []string{assets.CollectionReaderRole}},
		{UserID: "role-user", Roles: []string{assets.CollectionReaderRole, "team"}},
	} {
		item, err := store.GetAuthorizedCollectionItem(ctx, "reader-item-collection", "reader-item", subject)
		if err != nil || item.ID != "reader-item" || item.RemoteItemID != "remote-reader" || item.ETag != "etag-reader-item-asset" {
			t.Fatalf("subject=%+v item=%+v err=%v", subject, item, err)
		}
	}
	for _, subject := range []assets.CollectionSubject{
		{UserID: "user", Roles: []string{"manager"}},
		{UserID: "manager-only", Roles: []string{assets.CollectionReaderRole}},
	} {
		if _, err := store.GetAuthorizedCollectionItem(ctx, "reader-item-collection", "reader-item", subject); !errors.Is(err, assets.ErrForbidden) {
			t.Fatalf("subject=%+v err=%v", subject, err)
		}
	}
	if _, err := store.GetAuthorizedCollectionItem(ctx, "reader-item-collection", "missing", assets.CollectionSubject{UserID: "user", Roles: []string{assets.CollectionReaderRole}}); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("missing item err=%v", err)
	}
	if _, err := store.GetAuthorizedCollectionItem(ctx, "other-reader-collection", "reader-item", assets.CollectionSubject{UserID: "other", Roles: []string{assets.CollectionReaderRole}}); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("other collection item err=%v", err)
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,remote_item_id,display_name,source_revision,created_revision,deleted_revision,retention_exempt,updated_revision,created_at,updated_at,deleted_at) VALUES('deleted-reader-item','reader-item-collection','remote-deleted','Deleted','source',2,3,false,2,$1,$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetAuthorizedCollectionItem(ctx, "reader-item-collection", "deleted-reader-item", assets.CollectionSubject{UserID: "user", Roles: []string{assets.CollectionReaderRole}}); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("deleted item err=%v", err)
	}
	insertAsset(t, db, "pending-reader-asset", assets.UploadCompleted, assets.ScanPending, assets.ProcessingReady, now, time.Time{})
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES('pending-reader-item','reader-item-collection','pending-reader-asset','remote-pending','Pending','source',2,false,2,$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetAuthorizedCollectionItem(ctx, "reader-item-collection", "pending-reader-item", assets.CollectionSubject{UserID: "user", Roles: []string{assets.CollectionReaderRole}}); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("non-live asset item err=%v", err)
	}
	if _, err := db.Exec(`UPDATE asset_collection_acl SET revoked_at=$1 WHERE id='acl-reader-item-collection'`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetAuthorizedCollectionItem(ctx, "reader-item-collection", "reader-item", assets.CollectionSubject{UserID: "user", Roles: []string{assets.CollectionReaderRole}}); !errors.Is(err, assets.ErrForbidden) {
		t.Fatalf("revoked ACL err=%v", err)
	}
	if _, err := db.Exec(`UPDATE asset_collections SET deleted_at=$1 WHERE id='reader-item-collection'`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetAuthorizedCollectionItem(ctx, "reader-item-collection", "reader-item", assets.CollectionSubject{UserID: "role-user", Roles: []string{assets.CollectionReaderRole, "team"}}); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("deleted collection err=%v", err)
	}
}

func TestCollectionContentTicketLifecycleAndLiveRevocation(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 16, 6, 30, 0, 0, time.UTC)
	insertAuthorizedCollection(t, db, "ticket-live-collection", 2, "ticket-user", now)
	insertAsset(t, db, "ticket-live-asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	if _, err := db.Exec(`UPDATE assets SET original_file_name='video.mp4',detected_mime_type='video/mp4',size_bytes=6 WHERE id='ticket-live-asset'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES('ticket-live-item','ticket-live-collection','ticket-live-asset','remote-ticket','Video','source',2,false,2,$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_acl(id,collection_id,subject_type,subject_id,permission,created_at) VALUES('ticket-role-acl','ticket-live-collection','role','media-team','read',$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO asset_content_tickets(token_hash,collection_id,collection_item_id,asset_etag,user_id,roles,expires_at,created_at) VALUES($1,'ticket-live-collection','ticket-live-item','etag-ticket-live-asset','ticket-user',ARRAY[$2]::text[],$3,$4)`, strings.Repeat("a", 64), assets.CollectionReaderRole, now.Add(-time.Second), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	newTicket := func(hash, user string, roles []string) assets.ContentTicket {
		return assets.ContentTicket{
			TokenHash: hash, CollectionID: "ticket-live-collection", CollectionItemID: "ticket-live-item",
			AssetETag: "etag-ticket-live-asset", UserID: user, Roles: roles,
			ExpiresAt: now.Add(5 * time.Minute), CreatedAt: now,
		}
	}
	userTicket := newTicket(strings.Repeat("b", 64), "ticket-user", []string{assets.CollectionReaderRole})
	if err := store.CreateContentTicket(ctx, userTicket, now); err != nil {
		t.Fatal(err)
	}
	var storedHash string
	if err := db.QueryRow(`SELECT token_hash FROM asset_content_tickets WHERE token_hash=$1`, userTicket.TokenHash).Scan(&storedHash); err != nil || storedHash != userTicket.TokenHash {
		t.Fatalf("stored hash=%q err=%v", storedHash, err)
	}
	var expiredCount int
	if err := db.QueryRow(`SELECT count(*) FROM asset_content_tickets WHERE token_hash=$1`, strings.Repeat("a", 64)).Scan(&expiredCount); err != nil || expiredCount != 0 {
		t.Fatalf("expired count=%d err=%v", expiredCount, err)
	}
	asset, err := store.RedeemContentTicket(ctx, userTicket.TokenHash, now)
	if err != nil || asset.ID != "ticket-live-asset" || asset.ETag != userTicket.AssetETag || asset.ObjectKey != "assets/ticket-live-asset" {
		t.Fatalf("asset=%+v err=%v", asset, err)
	}

	roleTicket := newTicket(strings.Repeat("c", 64), "role-user", []string{assets.CollectionReaderRole, "media-team"})
	if err := store.CreateContentTicket(ctx, roleTicket, now); err != nil {
		t.Fatal(err)
	}
	var rolesPreserved bool
	if err := db.QueryRow(`SELECT roles=ARRAY[$2,$3]::text[] FROM asset_content_tickets WHERE token_hash=$1`, roleTicket.TokenHash, assets.CollectionReaderRole, "media-team").Scan(&rolesPreserved); err != nil || !rolesPreserved {
		t.Fatalf("roles preserved=%v err=%v", rolesPreserved, err)
	}
	if _, err := store.RedeemContentTicket(ctx, roleTicket.TokenHash, now); err != nil {
		t.Fatal(err)
	}
	freshUserTicket := newTicket(strings.Repeat("1", 64), "ticket-user", []string{assets.CollectionReaderRole})
	if err := store.CreateContentTicket(ctx, freshUserTicket, now); err != nil {
		t.Fatal(err)
	}
	assetStateTicket := newTicket(strings.Repeat("3", 64), "ticket-user", []string{assets.CollectionReaderRole})
	if err := store.CreateContentTicket(ctx, assetStateTicket, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE assets SET deleted_at=$1 WHERE id='ticket-live-asset'`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RedeemContentTicket(ctx, assetStateTicket.TokenHash, now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("deleted asset err=%v", err)
	}
	if _, err := db.Exec(`UPDATE assets SET deleted_at=NULL WHERE id='ticket-live-asset'`); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateContentTicket(ctx, newTicket(strings.Repeat("d", 64), "ticket-user", []string{"manager"}), now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("missing global role issue err=%v", err)
	}
	wrongVersion := newTicket(strings.Repeat("e", 64), "ticket-user", []string{assets.CollectionReaderRole})
	wrongVersion.AssetETag = "other-etag"
	if err := store.CreateContentTicket(ctx, wrongVersion, now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("wrong version issue err=%v", err)
	}
	overlong := newTicket(strings.Repeat("2", 64), "ticket-user", []string{assets.CollectionReaderRole})
	overlong.ExpiresAt = now.Add(5*time.Minute + time.Second)
	if err := store.CreateContentTicket(ctx, overlong, now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("overlong ticket err=%v", err)
	}

	if _, err := db.Exec(`UPDATE asset_content_tickets SET expires_at=$2 WHERE token_hash=$1`, userTicket.TokenHash, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RedeemContentTicket(ctx, userTicket.TokenHash, now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("expired ticket err=%v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM asset_content_tickets WHERE token_hash=$1`, userTicket.TokenHash).Scan(&expiredCount); err != nil || expiredCount != 0 {
		t.Fatalf("expired lookup count=%d err=%v", expiredCount, err)
	}

	if _, err := db.Exec(`UPDATE asset_collection_acl SET revoked_at=$1 WHERE id='ticket-role-acl'`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RedeemContentTicket(ctx, roleTicket.TokenHash, now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("revoked role ACL err=%v", err)
	}
	if _, err := db.Exec(`UPDATE asset_collection_acl SET revoked_at=$1 WHERE id='acl-ticket-live-collection'`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RedeemContentTicket(ctx, freshUserTicket.TokenHash, now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("revoked user ACL err=%v", err)
	}
}

func TestManagedContentTicketBypassesReaderACLButTracksCurrentItemAndAsset(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 19, 7, 0, 0, 0, time.UTC)
	collectionID := "managed-ticket-collection"
	itemID := "550e8400e29b41d4a716446655440000"
	assetID := "managed-ticket-asset"
	insertCollection(t, db, collectionID, now)
	insertAsset(t, db, assetID, assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	if _, err := db.Exec(`UPDATE assets SET original_file_name='stored.mp4',detected_mime_type='video/mp4',size_bytes=6 WHERE id=$1`, assetID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES($1,$2,$3,'remote','Original.mp4','source',1,false,1,$4,$4)`, itemID, collectionID, assetID, now); err != nil {
		t.Fatal(err)
	}
	item, err := store.GetManagedCollectionItem(ctx, collectionID, itemID, "hhc-line-function-bot")
	if err != nil || item.ETag != "etag-"+assetID {
		t.Fatalf("item=%+v err=%v", item, err)
	}
	service := assets.NewService(store, nil, "", func() time.Time { return now })
	issued, err := service.IssueManagedContentTickets(ctx, collectionID, "hhc-line-function-bot", []string{itemID}, time.Minute)
	if err != nil || len(issued.Tickets) != 1 {
		t.Fatalf("owned ticket=%+v err=%v", issued, err)
	}
	if _, err := service.IssueManagedContentTickets(ctx, collectionID, "other-service", []string{itemID}, time.Minute); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("cross-owner ticket err=%v", err)
	}
	if _, err := store.GetManagedCollectionItem(ctx, collectionID, itemID, "other-service"); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("cross-owner item err=%v", err)
	}
	if _, err := db.Exec(`UPDATE asset_collections SET namespace='other' WHERE id=$1`, collectionID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetManagedCollectionItem(ctx, collectionID, itemID, "hhc-line-function-bot"); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("cross-namespace item err=%v", err)
	}
	if _, err := service.IssueManagedContentTickets(ctx, collectionID, "hhc-line-function-bot", []string{itemID}, time.Minute); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("cross-namespace ticket err=%v", err)
	}
	if _, err := db.Exec(`UPDATE asset_collections SET namespace='line.group.media-sync' WHERE id=$1`, collectionID); err != nil {
		t.Fatal(err)
	}
	newTicket := func(hash string) assets.ContentTicket {
		return assets.ContentTicket{TokenHash: hash, CollectionID: collectionID, CollectionItemID: itemID, AssetETag: item.ETag, UserID: "manager", AccessMode: "manager", ExpiresAt: now.Add(5 * time.Minute), CreatedAt: now}
	}
	first := newTicket(strings.Repeat("a", 64))
	if err := store.CreateContentTicket(ctx, first, now); err != nil {
		t.Fatalf("manager ticket without reader ACL: %v", err)
	}
	var storedRoles string
	if err := db.QueryRow(`SELECT roles::text FROM asset_content_tickets WHERE token_hash=$1`, first.TokenHash).Scan(&storedRoles); err != nil || storedRoles != "{}" {
		t.Fatalf("stored roles=%q err=%v", storedRoles, err)
	}
	if _, err := db.Exec(`UPDATE asset_collection_items SET display_name='renamed.mp4' WHERE id=$1`, itemID); err != nil {
		t.Fatal(err)
	}
	asset, err := store.RedeemContentTicket(ctx, first.TokenHash, now)
	if err != nil || asset.OriginalFileName != "renamed.mp4" {
		t.Fatalf("asset=%+v err=%v", asset, err)
	}
	if _, err := db.Exec(`UPDATE assets SET etag='changed' WHERE id=$1`, assetID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RedeemContentTicket(ctx, first.TokenHash, now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("changed ETag err=%v", err)
	}
	if _, err := db.Exec(`UPDATE assets SET etag=$2 WHERE id=$1`, assetID, item.ETag); err != nil {
		t.Fatal(err)
	}
	second := newTicket(strings.Repeat("b", 64))
	if err := store.CreateContentTicket(ctx, second, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE asset_collection_items SET deleted_revision=2,deleted_at=$2 WHERE id=$1`, itemID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RedeemContentTicket(ctx, second.TokenHash, now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("deleted item err=%v", err)
	}
}

func TestCollectionContentTicketPinsOccurrenceCollectionAndAssetVersion(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 16, 7, 0, 0, 0, time.UTC)
	insertAuthorizedCollection(t, db, "ticket-pin-collection", 2, "user", now)
	insertAsset(t, db, "ticket-pin-asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES('ticket-pin-item','ticket-pin-collection','ticket-pin-asset','same-remote','Pinned','source',2,false,2,$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	ticket := assets.ContentTicket{
		TokenHash: strings.Repeat("f", 64), CollectionID: "ticket-pin-collection", CollectionItemID: "ticket-pin-item",
		AssetETag: "etag-ticket-pin-asset", UserID: "user", Roles: []string{assets.CollectionReaderRole},
		ExpiresAt: now.Add(5 * time.Minute), CreatedAt: now,
	}
	if err := store.CreateContentTicket(ctx, ticket, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE assets SET etag='replacement-etag' WHERE id='ticket-pin-asset'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RedeemContentTicket(ctx, ticket.TokenHash, now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("replaced content err=%v", err)
	}
	if _, err := db.Exec(`UPDATE assets SET etag='etag-ticket-pin-asset' WHERE id='ticket-pin-asset'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE asset_collection_items SET deleted_revision=3,deleted_at=$1 WHERE id='ticket-pin-item'`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES('ticket-pin-item-new','ticket-pin-collection','ticket-pin-asset','same-remote','Re-added','source-2',4,false,4,$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RedeemContentTicket(ctx, ticket.TokenHash, now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("deleted and re-added occurrence err=%v", err)
	}
	if _, err := db.Exec(`UPDATE asset_collections SET deleted_at=$1 WHERE id='ticket-pin-collection'`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RedeemContentTicket(ctx, ticket.TokenHash, now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("deleted collection err=%v", err)
	}
}

func TestCollectionChangesResetDeltaAndCursorRecovery(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 16, 6, 0, 0, 0, time.UTC)
	collectionID := "changes-collection"
	insertAuthorizedCollection(t, db, collectionID, 502, "user", now)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 501; index++ {
		if _, err := tx.Exec(`INSERT INTO asset_collection_items(id,collection_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES($1,$2,$3,$3,'source',$4,false,$4,$5,$5)`, fmt.Sprintf("item-%04d", index), collectionID, fmt.Sprintf("remote-%04d", index), index+1, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	subject := assets.CollectionSubject{UserID: "user", Roles: []string{assets.CollectionReaderRole}}

	first, err := store.CollectionChanges(ctx, collectionID, "", subject)
	if err != nil || !first.Reset || !first.HasMore || len(first.Items) != 500 || len(first.Tombstones) != 0 {
		t.Fatalf("first reset items=%d tombstones=%d reset=%v more=%v err=%v", len(first.Items), len(first.Tombstones), first.Reset, first.HasMore, err)
	}
	insertAsset(t, db, "mid-reset-asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	if _, err := db.Exec("UPDATE assets SET namespace='namespace',owner_service='helper' WHERE id='mid-reset-asset'"); err != nil {
		t.Fatal(err)
	}
	mid, err := store.AddCollectionItem(ctx, assets.AddCollectionItemInput{CollectionID: collectionID, AssetID: "mid-reset-asset", RemoteItemID: "mid-reset", DisplayName: "Mid reset", SourceRevision: "source-mid", CallerService: "helper", IdempotencyKey: "mid-reset"}, now.Add(time.Minute))
	if err != nil || mid.Collection.Revision != 503 {
		t.Fatalf("mid reset mutation=%+v err=%v", mid, err)
	}
	second, err := store.CollectionChanges(ctx, collectionID, first.Cursor, subject)
	if err != nil || !second.Reset || !second.HasMore || len(second.Items) != 1 {
		t.Fatalf("second reset items=%d reset=%v more=%v err=%v", len(second.Items), second.Reset, second.HasMore, err)
	}
	delta, err := store.CollectionChanges(ctx, collectionID, second.Cursor, subject)
	if err != nil || delta.Reset || delta.HasMore || len(delta.Items) != 1 || delta.Items[0].ID != mid.Item.ID {
		t.Fatalf("delta=%+v err=%v", delta, err)
	}
	aclGap, err := store.AddCollectionACL(ctx, assets.AddCollectionACLInput{CollectionID: collectionID, SubjectType: assets.SubjectRole, SubjectID: "gap", Permission: assets.PermissionRead, CallerService: "helper", IdempotencyKey: "gap"}, now.Add(2*time.Minute))
	if err != nil || aclGap.Collection.Revision != 504 {
		t.Fatalf("ACL gap=%+v err=%v", aclGap, err)
	}
	gap, err := store.CollectionChanges(ctx, collectionID, delta.Cursor, subject)
	if err != nil || gap.Reset || gap.HasMore || len(gap.Items) != 0 || len(gap.Tombstones) != 0 || gap.Collection.Revision != 504 {
		t.Fatalf("gap=%+v err=%v", gap, err)
	}

	malformed, err := store.CollectionChanges(ctx, collectionID, "not-a-cursor", subject)
	if err != nil || !malformed.Reset || len(malformed.Items) != 500 {
		t.Fatalf("malformed reset=%v items=%d err=%v", malformed.Reset, len(malformed.Items), err)
	}
	insertAuthorizedCollection(t, db, "other-changes", 1, "user", now)
	wrong, err := store.CollectionChanges(ctx, "other-changes", first.Cursor, subject)
	if err != nil || !wrong.Reset {
		t.Fatalf("wrong collection cursor page=%+v err=%v", wrong, err)
	}
	aheadCursor := encodeChangeCursor(changeCursor{Mode: changeModeDelta, CollectionID: collectionID, FromRevision: 999, ToRevision: 999})
	ahead, err := store.CollectionChanges(ctx, collectionID, aheadCursor, subject)
	if err != nil || !ahead.Reset || len(ahead.Items) != 500 {
		t.Fatalf("ahead reset=%v items=%d err=%v", ahead.Reset, len(ahead.Items), err)
	}
}

func TestCollectionChangesFinalResetAlwaysHandsOffToDelta(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	now := time.Date(2026, 8, 16, 6, 30, 0, 0, time.UTC)
	collectionID := "reset-handoff"
	insertAuthorizedCollection(t, db, collectionID, 2, "user", now)
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES('reset-item',$1,'remote-reset','Reset','source',2,false,2,$2,$2)`, collectionID, now); err != nil {
		t.Fatal(err)
	}
	subject := assets.CollectionSubject{UserID: "user", Roles: []string{assets.CollectionReaderRole}}

	reset, err := store.CollectionChanges(context.Background(), collectionID, "", subject)
	if err != nil || !reset.Reset || !reset.HasMore || len(reset.Items) != 1 {
		t.Fatalf("reset=%+v err=%v", reset, err)
	}
	delta, err := store.CollectionChanges(context.Background(), collectionID, reset.Cursor, subject)
	if err != nil || delta.Reset || delta.HasMore || len(delta.Items) != 0 || len(delta.Tombstones) != 0 {
		t.Fatalf("delta=%+v err=%v", delta, err)
	}
}

func TestCollectionChangesLimitsCombinedDeltaEvents(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	now := time.Date(2026, 8, 16, 7, 0, 0, 0, time.UTC)
	collectionID := "delta-limit"
	insertAuthorizedCollection(t, db, collectionID, 503, "user", now)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 251; index++ {
		if _, err := tx.Exec(`INSERT INTO asset_collection_items(id,collection_id,remote_item_id,display_name,source_revision,created_revision,deleted_revision,retention_exempt,updated_revision,created_at,updated_at,deleted_at) VALUES($1,$2,$3,$3,'source',$4,$5,false,$4,$6,$6,$6)`, fmt.Sprintf("delta-%04d", index), collectionID, fmt.Sprintf("remote-%04d", index), index+1, index+252, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	subject := assets.CollectionSubject{UserID: "user", Roles: []string{assets.CollectionReaderRole}}
	cursor := encodeChangeCursor(changeCursor{Mode: changeModeDelta, CollectionID: collectionID, FromRevision: 1, ToRevision: 503})
	first, err := store.CollectionChanges(context.Background(), collectionID, cursor, subject)
	if err != nil || first.Reset || !first.HasMore || len(first.Items)+len(first.Tombstones) != 500 {
		t.Fatalf("first delta items=%d tombstones=%d reset=%v more=%v err=%v", len(first.Items), len(first.Tombstones), first.Reset, first.HasMore, err)
	}
	second, err := store.CollectionChanges(context.Background(), collectionID, first.Cursor, subject)
	if err != nil || second.Reset || second.HasMore || len(second.Items)+len(second.Tombstones) != 2 {
		t.Fatalf("second delta items=%d tombstones=%d reset=%v more=%v err=%v", len(second.Items), len(second.Tombstones), second.Reset, second.HasMore, err)
	}
}

func TestBatchRetentionUpdatesMetadataWithoutReaderRevision(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	createdAt := time.Date(2026, 8, 19, 3, 0, 0, 0, time.UTC)
	updatedAt := createdAt.Add(time.Minute)
	insertCollection(t, db, "batch-retention", createdAt)
	activeID := "550e8400e29b41d4a716446655440001"
	deletedID := "550e8400e29b41d4a716446655440002"
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES($1,'batch-retention','active','Active','source',1,false,1,$3,$3),($2,'batch-retention','deleted','Deleted','source',1,false,1,$3,$3)`, activeID, deletedID, createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE asset_collection_items SET deleted_revision=2,deleted_at=$2 WHERE id=$1`, deletedID, createdAt); err != nil {
		t.Fatal(err)
	}
	input := assets.SetCollectionItemsRetentionInput{CollectionID: "batch-retention", ItemIDs: []string{deletedID, activeID}, RetentionExempt: true, CallerService: "hhc-line-function-bot", IdempotencyKey: "retention"}
	if err := store.SetCollectionItemsRetention(ctx, input, updatedAt); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCollectionItemsRetention(ctx, input, updatedAt.Add(time.Minute)); err != nil {
		t.Fatalf("replay: %v", err)
	}
	var revision, activeRevision int64
	var activeExempt, deletedExempt bool
	var activeUpdatedAt time.Time
	if err := db.QueryRow(`SELECT revision FROM asset_collections WHERE id='batch-retention'`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT retention_exempt,updated_revision,updated_at FROM asset_collection_items WHERE id=$1`, activeID).Scan(&activeExempt, &activeRevision, &activeUpdatedAt); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT retention_exempt FROM asset_collection_items WHERE id=$1`, deletedID).Scan(&deletedExempt); err != nil {
		t.Fatal(err)
	}
	if revision != 1 || !activeExempt || deletedExempt || activeRevision != 1 || !activeUpdatedAt.Equal(updatedAt) {
		t.Fatalf("revision=%d active=(%v,%d,%s) deletedExempt=%v", revision, activeExempt, activeRevision, activeUpdatedAt, deletedExempt)
	}
}

func TestBatchDeleteHandlesActiveDeletedReplayAndInvalidatesTicket(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	createdAt := time.Date(2026, 8, 19, 3, 30, 0, 0, time.UTC)
	deletedAt := createdAt.Add(time.Minute)
	insertCollection(t, db, "batch-delete", createdAt)
	activeID := "550e8400e29b41d4a716446655440011"
	secondActiveID := "550e8400e29b41d4a716446655440014"
	deletedID := "550e8400e29b41d4a716446655440012"
	missingID := "550e8400e29b41d4a716446655440013"
	insertAsset(t, db, "batch-delete-asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, createdAt, time.Time{})
	if _, err := db.Exec(`UPDATE assets SET namespace='cms.news.cover',original_file_name='video.mp4',detected_mime_type='video/mp4',size_bytes=6 WHERE id='batch-delete-asset'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES($1,'batch-delete','batch-delete-asset','active','Active.mp4','source',1,true,1,$4,$4),($2,'batch-delete',NULL,'second-active','Second.mp4','source',1,false,1,$4,$4),($3,'batch-delete',NULL,'deleted','Deleted.mp4','source',1,false,1,$4,$4)`, activeID, secondActiveID, deletedID, createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE asset_collection_items SET deleted_revision=2,deleted_at=$2 WHERE id=$1`, deletedID, createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE asset_collections SET revision=2 WHERE id='batch-delete'`); err != nil {
		t.Fatal(err)
	}
	ticket := assets.ContentTicket{TokenHash: strings.Repeat("f", 64), CollectionID: "batch-delete", CollectionItemID: activeID, AssetETag: "etag-batch-delete-asset", UserID: "user", Roles: []string{assets.CollectionReaderRole}, ExpiresAt: createdAt.Add(5 * time.Minute), CreatedAt: createdAt}
	if _, err := db.Exec(`INSERT INTO asset_collection_acl(id,collection_id,subject_type,subject_id,permission,created_at) VALUES('batch-delete-acl','batch-delete','user','user','read',$1)`, createdAt); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateContentTicket(ctx, ticket, createdAt); err != nil {
		t.Fatal(err)
	}
	input := assets.DeleteCollectionItemsInput{CollectionID: "batch-delete", ItemIDs: []string{missingID, deletedID, secondActiveID, activeID}, CallerService: "hhc-line-function-bot", IdempotencyKey: "delete"}
	result, err := store.DeleteCollectionItems(ctx, input, deletedAt)
	if err != nil || result != (assets.DeleteCollectionItemsResult{Deleted: 2, AlreadyRemoved: 2}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	replay, err := store.DeleteCollectionItems(ctx, input, deletedAt.Add(time.Minute))
	if err != nil || replay != result {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	var revision, activeDeletedRevision, secondDeletedRevision, oldDeletedRevision int64
	if err := db.QueryRow(`SELECT c.revision,active.deleted_revision,second_active.deleted_revision,removed.deleted_revision FROM asset_collections c JOIN asset_collection_items active ON active.collection_id=c.id AND active.id=$1 JOIN asset_collection_items second_active ON second_active.collection_id=c.id AND second_active.id=$2 JOIN asset_collection_items removed ON removed.collection_id=c.id AND removed.id=$3 WHERE c.id='batch-delete'`, activeID, secondActiveID, deletedID).Scan(&revision, &activeDeletedRevision, &secondDeletedRevision, &oldDeletedRevision); err != nil {
		t.Fatal(err)
	}
	if revision != 3 || activeDeletedRevision != 3 || secondDeletedRevision != 3 || oldDeletedRevision != 2 {
		t.Fatalf("revision=%d active=%d second=%d old=%d", revision, activeDeletedRevision, secondDeletedRevision, oldDeletedRevision)
	}
	if _, err := store.RedeemContentTicket(ctx, ticket.TokenHash, deletedAt); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("ticket remained valid: %v", err)
	}
	if _, err := store.RenameCollectionItem(ctx, assets.RenameCollectionItemInput{CollectionID: "batch-delete", ItemID: activeID, DisplayName: "Later.mp4", CallerService: "hhc-line-function-bot", IdempotencyKey: "rename-after-delete"}, deletedAt); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("rename after delete: %v", err)
	}
}

func TestBatchDeleteReferencedAssetLifecycle(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 19, 4, 0, 0, 0, time.UTC)
	insertCollection(t, db, "asset-delete-first", now)
	insertCollection(t, db, "asset-delete-second", now)
	assetIDs := []string{"eligible", "referenced", "wrong-namespace", "wrong-service", "wrong-type"}
	for _, assetID := range assetIDs {
		insertAsset(t, db, assetID, assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
		if _, err := db.Exec(`UPDATE assets SET namespace='line.group.media-sync',owner_service='hhc-line-function-bot',owner_type='media_sync_ingest' WHERE id=$1`, assetID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE assets SET namespace='other' WHERE id='wrong-namespace'; UPDATE assets SET owner_service='other' WHERE id='wrong-service'; UPDATE assets SET owner_type='other' WHERE id='wrong-type'`); err != nil {
		t.Fatal(err)
	}
	itemIDs := make([]string, 0, len(assetIDs))
	for index, assetID := range assetIDs {
		itemID := fmt.Sprintf("550e8400e29b41d4a7164466554401%02d", index)
		itemIDs = append(itemIDs, itemID)
		if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES($1,'asset-delete-first',$2,$2,$2,'source',1,false,1,$3,$3)`, itemID, assetID, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES('550e8400e29b41d4a716446655440199','asset-delete-second','referenced','referenced','Referenced','source',1,false,1,$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	result, err := store.DeleteCollectionItems(ctx, assets.DeleteCollectionItemsInput{CollectionID: "asset-delete-first", ItemIDs: itemIDs, CallerService: "hhc-line-function-bot", IdempotencyKey: "asset-delete"}, now.Add(time.Minute))
	if err != nil || result.Deleted != len(itemIDs) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, assetID := range assetIDs {
		var deletedAt sql.NullTime
		if err := db.QueryRow(`SELECT deleted_at FROM assets WHERE id=$1`, assetID).Scan(&deletedAt); err != nil {
			t.Fatal(err)
		}
		if (assetID == "eligible") != deletedAt.Valid {
			t.Fatalf("asset=%s deleted=%v", assetID, deletedAt.Valid)
		}
	}
	blobs := &recordingLifecycleBlobStore{}
	processed, err := lifecycle.NewWorker(store, blobs).ProcessOne(ctx)
	if err != nil || !processed || !blobs.deleted["assets/eligible"] {
		t.Fatalf("processed=%v blobs=%v err=%v", processed, blobs.deleted, err)
	}
}

func TestDeleteRaceRetentionExemptionWinsBeforeWorkerLock(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 19, 4, 30, 0, 0, time.UTC)
	insertCollection(t, db, "retention-race", now)
	itemID := "550e8400e29b41d4a716446655440201"
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES($1,'retention-race','remote','Media','source',1,false,1,$2,$2)`, itemID, now); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCollectionItemsRetention(ctx, assets.SetCollectionItemsRetentionInput{CollectionID: "retention-race", ItemIDs: []string{itemID}, RetentionExempt: true, CallerService: "hhc-line-function-bot", IdempotencyKey: "retain-before-worker"}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	deletion, err := store.deleteCollectionItems(ctx, tx, "retention-race", "hhc-line-function-bot", []string{itemID}, now.Add(2*time.Minute), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if deletion.result.Deleted != 0 || deletion.result.ExemptSkipped != 1 || deletion.result.AlreadyRemoved != 0 {
		t.Fatalf("deletion=%+v", deletion)
	}
	var deletedRevision sql.NullInt64
	if err := db.QueryRow(`SELECT deleted_revision FROM asset_collection_items WHERE id=$1`, itemID).Scan(&deletedRevision); err != nil {
		t.Fatal(err)
	}
	if deletedRevision.Valid {
		t.Fatalf("deleted revision=%d", deletedRevision.Int64)
	}
}

func TestExpiredCollectionItemsUseCurrentPolicyAndExactBoundary(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 19, 19, 0, 0, 0, time.UTC)
	insertCollection(t, db, "expired-items", now.Add(-48*time.Hour))
	if _, err := db.Exec(`UPDATE asset_collections SET retention_days=2 WHERE id='expired-items'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at,deleted_revision,deleted_at) VALUES
		('exact','expired-items','exact','Exact','source',1,false,1,$1,$1,NULL,NULL),
		('active','expired-items','active','Active','source',1,false,1,$2,$2,NULL,NULL),
		('exempt','expired-items','exempt','Exempt','source',1,true,1,$3,$3,NULL,NULL),
		('removed','expired-items','removed','Removed','source',1,false,1,$3,$3,2,$4)`, now.Add(-48*time.Hour), now.Add(-48*time.Hour+time.Microsecond), now.Add(-72*time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO asset_collections(id,namespace,name,retention_days,created_by_service,created_at,updated_at) VALUES('other-namespace','other','Other',1,'hhc-line-function-bot',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES('other-item','other-namespace','other','Other','source',1,false,1,$1,$1)`, now.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}

	candidates, err := store.ListExpiredCollectionItems(ctx, now, 100)
	if err != nil || len(candidates) != 1 || candidates[0].ItemID != "exact" {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	preview, err := store.PreviewExpiredCollectionItems(ctx, now)
	if err != nil || len(preview) != 1 || preview[0].CollectionID != "expired-items" || preview[0].CandidateCount != 1 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	operations, err := store.GetOperations(ctx, now)
	if err != nil || operations.ExpiredCollectionItems != 1 {
		t.Fatalf("operations=%+v err=%v", operations, err)
	}

	if _, err := db.Exec(`UPDATE asset_collections SET retention_days=1 WHERE id='expired-items'`); err != nil {
		t.Fatal(err)
	}
	candidates, err = store.ListExpiredCollectionItems(ctx, now, 100)
	if err != nil || len(candidates) != 2 {
		t.Fatalf("retroactive candidates=%+v err=%v", candidates, err)
	}
	if _, err := db.Exec(`UPDATE asset_collections SET retention_days=3 WHERE id='expired-items'`); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeleteExpiredCollectionItems(ctx, "expired-items", []string{"exact"}, now)
	if err != nil || deleted.Deleted != 0 || deleted.AlreadyRemoved != 1 {
		t.Fatalf("policy-race deletion=%+v err=%v", deleted, err)
	}
}

func TestCollectionAssetLockOrderAvoidsSameCollectionDeadlock(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 19, 5, 0, 0, 0, time.UTC)
	insertCollection(t, db, "lock-order-collection", now)
	insertAsset(t, db, "lock-order-asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	if _, err := db.Exec(`UPDATE assets SET namespace='line.group.media-sync',owner_service='hhc-line-function-bot' WHERE id='lock-order-asset'`); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(`SELECT id FROM asset_collections WHERE id='lock-order-collection' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	added := make(chan error, 1)
	go func() {
		_, err := store.AddCollectionItem(context.Background(), assets.AddCollectionItemInput{
			CollectionID: "lock-order-collection", AssetID: "lock-order-asset", RemoteItemID: "remote",
			DisplayName: "Media", SourceRevision: "source", CallerService: "hhc-line-function-bot", IdempotencyKey: "lock-order-add",
		}, now)
		added <- err
	}()
	select {
	case err := <-added:
		t.Fatalf("add did not wait for collection lock: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if _, err := blocker.Exec(`SELECT id FROM assets WHERE id='lock-order-asset' FOR UPDATE`); err != nil {
		blocker.Rollback()
		t.Fatalf("asset lock deadlocked after collection lock: %v", err)
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-added; err != nil {
		t.Fatalf("add after lock release: %v", err)
	}
}

func TestBatchDeleteRechecksReferencesAfterCandidateAssetLock(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 19, 5, 30, 0, 0, time.UTC)
	insertCollection(t, db, "reference-delete", now)
	insertCollection(t, db, "reference-add", now)
	insertAsset(t, db, "reference-race-asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	if _, err := db.Exec(`UPDATE assets SET namespace='line.group.media-sync',owner_service='hhc-line-function-bot',owner_type='media_sync_ingest' WHERE id='reference-race-asset'`); err != nil {
		t.Fatal(err)
	}
	deletedItemID := "550e8400e29b41d4a716446655440301"
	addedItemID := "550e8400e29b41d4a716446655440302"
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES($1,'reference-delete','reference-race-asset','delete','Delete','source',1,false,1,$2,$2)`, deletedItemID, now); err != nil {
		t.Fatal(err)
	}
	adding, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adding.Exec(`SELECT id FROM asset_collections WHERE id='reference-add' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := adding.Exec(`SELECT id FROM assets WHERE id='reference-race-asset' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := adding.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,retention_exempt,updated_revision,created_at,updated_at) VALUES($1,'reference-add','reference-race-asset','add','Add','source',1,false,1,$2,$2)`, addedItemID, now); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() {
		_, err := store.DeleteCollectionItems(context.Background(), assets.DeleteCollectionItemsInput{
			CollectionID: "reference-delete", ItemIDs: []string{deletedItemID}, CallerService: "hhc-line-function-bot", IdempotencyKey: "reference-delete",
		}, now.Add(time.Minute))
		deleted <- err
	}()
	select {
	case err := <-deleted:
		t.Fatalf("delete did not wait for candidate asset lock: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := adding.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	var deletedAt sql.NullTime
	var active int
	if err := db.QueryRow(`SELECT deleted_at FROM assets WHERE id='reference-race-asset'`).Scan(&deletedAt); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM asset_collection_items WHERE asset_id='reference-race-asset' AND deleted_revision IS NULL`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if deletedAt.Valid || active != 1 {
		t.Fatalf("deleted=%v active=%d", deletedAt.Valid, active)
	}
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

type recordingLifecycleBlobStore struct {
	deleted map[string]bool
}

func (b *recordingLifecycleBlobStore) Delete(_ context.Context, key string) error {
	if b.deleted == nil {
		b.deleted = map[string]bool{}
	}
	b.deleted[key] = true
	return nil
}

func insertAsset(t *testing.T, db *sql.DB, id string, upload assets.UploadStatus, scan assets.ScanStatus, processing assets.ProcessingStatus, updatedAt, purgedAt time.Time) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO assets(
			id,namespace,owner_service,owner_type,owner_id,object_key,expected_mime_type,
			upload_status,scan_status,processing_status,visibility,created_at,updated_at,purged_at,etag,scan_event_id
		) VALUES($1,'cms.news.cover','test','item',$1,$2,'image/png',$3,$4,$5,'public',$6,$6,NULLIF($7,'0001-01-01'::timestamptz),$8,$9)`,
		id, "assets/"+id, upload, scan, processing, updatedAt, purgedAt, "etag-"+id, "event-"+id)
	if err != nil {
		t.Fatal(err)
	}
}

func insertCollection(t *testing.T, db *sql.DB, id string, now time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO asset_collections(id,namespace,name,created_by_service,created_at,updated_at) VALUES($1,'line.group.media-sync','Media','hhc-line-function-bot',$2,$2)`, id, now); err != nil {
		t.Fatal(err)
	}
}

func insertAuthorizedCollection(t *testing.T, db *sql.DB, id string, revision int64, userID string, now time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO asset_collections(id,namespace,name,revision,created_by_service,created_at,updated_at) VALUES($1,'namespace','Media',$2,'helper',$3,$3)`, id, revision, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_acl(id,collection_id,subject_type,subject_id,permission,created_at) VALUES($1,$2,'user',$3,'read',$4)`, "acl-"+id, id, userID, now); err != nil {
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
