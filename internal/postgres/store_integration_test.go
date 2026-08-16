package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
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
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,remote_item_id,display_name,source_revision,created_revision,created_at) VALUES('item-zero','collection','remote-zero','Zero','source',0,$1)`, now); err == nil {
		t.Fatal("zero item revision was accepted")
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,remote_item_id,display_name,source_revision,created_revision,deleted_revision,created_at,deleted_at) VALUES('item-reversed','collection','remote-reversed','Reversed','source',3,2,$1,$1)`, now); err == nil {
		t.Fatal("deleted revision before created revision was accepted")
	}
}

func TestCollectionSchemaStableItemOccurrencesAndActiveUniqueness(t *testing.T) {
	db := integrationDB(t)
	now := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	insertAsset(t, db, "collection-asset-1", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	insertAsset(t, db, "collection-asset-2", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	insertCollection(t, db, "collection", now)

	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,created_at) VALUES('','collection','collection-asset-1','remote-blank','Blank','source',1,$1)`, now); err == nil {
		t.Fatal("blank item occurrence ID was accepted")
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,created_at) VALUES('item-1','collection','collection-asset-1','remote-1','One','source-1',1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,created_at) VALUES('duplicate-asset','collection','collection-asset-1','remote-2','Duplicate asset','source-2',2,$1)`, now); err == nil {
		t.Fatal("duplicate active asset membership was accepted")
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,created_at) VALUES('duplicate-remote','collection','collection-asset-2','remote-1','Duplicate remote','source-2',2,$1)`, now); err == nil {
		t.Fatal("duplicate active remote item was accepted")
	}
	if _, err := db.Exec(`UPDATE asset_collection_items SET deleted_revision=2,deleted_at=$1 WHERE id='item-1'`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,created_at) VALUES('item-2','collection','collection-asset-1','remote-1','One again','source-2',3,$1)`, now); err != nil {
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
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,created_at) VALUES('ticket-item','collection','ticket-asset','remote-ticket','Ticket','source',1,$1)`, now); err != nil {
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
	if _, err := db.Exec(`INSERT INTO asset_collection_items(id,collection_id,asset_id,remote_item_id,display_name,source_revision,created_revision,deleted_revision,created_at,deleted_at) VALUES('retained-item','collection','retained-collection-asset','remote-retained','Retained','source',1,2,$1,$1)`, now); err != nil {
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

func TestCollectionMutationsReplayConflictAndRevision(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 16, 1, 0, 0, 0, time.UTC)
	insertAsset(t, db, "collection-mutation-asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})

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

func TestCollectionMutationsRejectNonOwner(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Date(2026, 8, 16, 2, 0, 0, 0, time.UTC)
	insertAsset(t, db, "owner-asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
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
	if err := store.SoftDeleteAsset(ctx, "delete-asset", "test", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.SoftDeleteAsset(ctx, "delete-asset", "test", now.Add(2*time.Minute)); err != nil {
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

	if err := store.CompletePurge(ctx, "delete-asset", now.Add(-181*24*time.Hour)); err != nil {
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

	insertAsset(t, db, "race-asset", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
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
		errorsCh <- store.SoftDeleteAsset(context.Background(), "race-asset", "test", now)
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
		if _, err := tx.Exec(`INSERT INTO asset_collection_items(id,collection_id,remote_item_id,display_name,source_revision,created_revision,created_at) VALUES($1,$2,$3,$3,'source',$4,$5)`, fmt.Sprintf("item-%04d", index), collectionID, fmt.Sprintf("remote-%04d", index), index+1, now); err != nil {
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
		if _, err := tx.Exec(`INSERT INTO asset_collection_items(id,collection_id,remote_item_id,display_name,source_revision,created_revision,deleted_revision,created_at,deleted_at) VALUES($1,$2,$3,$3,'source',$4,$5,$6,$6)`, fmt.Sprintf("delta-%04d", index), collectionID, fmt.Sprintf("remote-%04d", index), index+1, index+252, now); err != nil {
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
