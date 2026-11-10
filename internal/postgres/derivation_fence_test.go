package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestTrustedDerivationTransactionFencesSourceAndReplay(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("proof-enabled-%t", enabled), func(t *testing.T) { testTrustedDerivationFence(t, enabled) })
	}
}

func testTrustedDerivationFence(t *testing.T, enabled bool) {
	db := integrationDB(t)
	store := New(db).WithDerivationProofEnabled(enabled)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	now := time.Now().UTC()
	for _, mode := range []string{"replay", "source deleted", "source quarantined", "wrong digest", "wrong etag", "ordinary won"} {
		t.Run(mode, func(t *testing.T) {
			sourceID, outputID := "source-"+mode, "output-"+mode
			hash := strings.Repeat("c", 64)
			insertAsset(t, db, sourceID, assets.UploadCompleted, assets.ScanClean, assets.ProcessingNotRequired, now, time.Time{})
			if _, err := db.ExecContext(ctx, `UPDATE assets SET namespace='cms.weekly.pdf',owner_service='hhc-web-api',owner_type='bulletin_issue',purpose='weekly_bulletin',locale='en',visibility='private',detected_mime_type='application/pdf',checksum_sha256=$2,etag='source-etag' WHERE id=$1`, sourceID, hash); err != nil {
				t.Fatal(err)
			}
			asset := assets.Asset{
				ID: outputID, Namespace: "cms.weekly.pdf", OwnerService: "hhc-web-api", OwnerType: "bulletin_download_job", OwnerID: "job",
				Purpose: "weekly_bulletin_member", Locale: "en", ObjectKey: "test/" + outputID, ExpectedMIMEType: "application/pdf",
				UploadStatus: assets.UploadCreated, ScanStatus: assets.ScanPending, ProcessingStatus: assets.ProcessingNotRequired,
				Visibility: assets.VisibilityPrivate, CreatedAt: now, UpdatedAt: now,
			}
			session := assets.UploadSession{ID: "session-" + mode, AssetID: asset.ID, IdempotencyKey: mode, CallerService: asset.OwnerService, Operation: "create_upload", StagingObjectKey: "staging/" + mode, MaxSizeBytes: 1024, Status: assets.UploadCreated, CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
			if err := store.CreateUpload(ctx, asset, session); err != nil {
				t.Fatal(err)
			}
			asset.UploadStatus, asset.ScanStatus, asset.ScanSignature = assets.UploadCompleted, assets.ScanClean, assets.TrustedBulletinWatermark
			asset.ScanDetails, asset.DetectedMIMEType, asset.ChecksumSHA256, asset.ETag, asset.SizeBytes = sourceID+":"+hash, "application/pdf", strings.Repeat("e", 64), "output-etag", 20
			asset.Derivation = assets.Derivation{SourceAssetID: sourceID, SourceSHA256: hash, SourceETag: "source-etag", Transform: "bulletin-watermark", TransformVersion: "1"}
			session.Status, session.CompletedAt = assets.UploadCompleted, now
			switch mode {
			case "source deleted", "source quarantined":
				block, err := db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer block.Rollback()
				update := "UPDATE assets SET deleted_at=now() WHERE id=$1"
				if mode == "source quarantined" {
					update = "UPDATE assets SET scan_status='infected' WHERE id=$1"
				}
				if _, err = block.ExecContext(ctx, update, sourceID); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- store.CompleteUpload(ctx, asset, session, assets.ScanRequest{}) }()
				waitForCompletionLock(t, ctx, db, "%ORDER BY id FOR UPDATE%", done)
				if err = block.Commit(); err != nil {
					t.Fatal(err)
				}
				if err = <-done; !errors.Is(err, assets.ErrInvalidInput) {
					t.Fatalf("source race accepted: %v", err)
				}
			case "replay":
				if err := store.CompleteUpload(ctx, asset, session, assets.ScanRequest{}); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, "UPDATE assets SET deleted_at=now() WHERE id=$1", sourceID); err != nil {
					t.Fatal(err)
				}
				if err := store.CompleteUpload(ctx, asset, session, assets.ScanRequest{}); (enabled && err != nil) || (!enabled && !errors.Is(err, assets.ErrConflict)) {
					t.Fatalf("replay after source deletion (proof enabled=%t): %v", enabled, err)
				}
				changed := asset
				changed.Derivation.SourceETag = "other-version"
				if err := store.CompleteUpload(ctx, changed, session, assets.ScanRequest{}); !errors.Is(err, assets.ErrConflict) {
					t.Fatalf("different proof replay: %v", err)
				}
			case "ordinary won":
				ordinary := asset
				ordinary.Derivation = assets.Derivation{}
				ordinary.ScanStatus, ordinary.ScanSignature, ordinary.ScanDetails = assets.ScanPending, "", ""
				request := assets.ScanRequest{EventID: "ordinary-event", AssetID: asset.ID, ETag: asset.ETag, CreatedAt: now}
				// Pause the real ordinary transaction after it has acquired the output lock.
				if _, err := db.ExecContext(ctx, `
					CREATE FUNCTION pause_ordinary_completion() RETURNS trigger LANGUAGE plpgsql AS $$
					BEGIN PERFORM pg_advisory_xact_lock(707404); RETURN NEW; END $$;
					CREATE TRIGGER pause_ordinary_completion BEFORE UPDATE ON assets FOR EACH ROW
					WHEN (NEW.id = 'output-ordinary won' AND NEW.upload_status = 'completed' AND NEW.scan_status = 'pending')
					EXECUTE FUNCTION pause_ordinary_completion()`); err != nil {
					t.Fatal(err)
				}
				block, err := db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer block.Rollback()
				if _, err = block.ExecContext(ctx, `SELECT pg_advisory_xact_lock(707404)`); err != nil {
					t.Fatal(err)
				}
				ordinaryDone, derivedDone := make(chan error, 1), make(chan error, 1)
				go func() { ordinaryDone <- store.CompleteUpload(ctx, ordinary, session, request) }()
				waitForCompletionLock(t, ctx, db, "UPDATE assets SET detected_mime_type=%", ordinaryDone)
				go func() { derivedDone <- store.CompleteUpload(ctx, asset, session, assets.ScanRequest{}) }()
				waitForCompletionLock(t, ctx, db, "%ORDER BY id FOR UPDATE%", derivedDone)
				if err = block.Commit(); err != nil {
					t.Fatal(err)
				}
				if err = <-ordinaryDone; err != nil {
					t.Fatal(err)
				}
				if err = <-derivedDone; !errors.Is(err, assets.ErrConflict) {
					t.Fatalf("ordinary completion was promoted: %v", err)
				}
			default:
				if mode == "wrong digest" {
					asset.Derivation.SourceSHA256 = strings.Repeat("d", 64)
					asset.ScanDetails = sourceID + ":" + asset.Derivation.SourceSHA256
				} else {
					asset.Derivation.SourceETag = "other-version"
				}
				if err := store.CompleteUpload(ctx, asset, session, assets.ScanRequest{}); !errors.Is(err, assets.ErrInvalidInput) {
					t.Fatalf("invalid source proof accepted: %v", err)
				}
			}
			stored, err := store.GetAsset(ctx, asset.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "replay":
				expected := assets.Derivation{}
				if enabled {
					expected = asset.Derivation
				}
				if stored.Derivation != expected || stored.ScanStatus != assets.ScanClean {
					t.Fatalf("wrong stored evidence: %+v", stored)
				}
			case "ordinary won":
				if stored.Derivation != (assets.Derivation{}) || stored.ScanStatus != assets.ScanPending || stored.UploadStatus != assets.UploadCompleted {
					t.Fatalf("ordinary evidence changed: %+v", stored)
				}
			default:
				if stored.Derivation != (assets.Derivation{}) || stored.UploadStatus != assets.UploadCreated {
					t.Fatalf("rejected proof changed output: %+v", stored)
				}
			}
		})
	}
}

func waitForCompletionLock(t *testing.T, ctx context.Context, db *sql.DB, statement string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var waiting bool
		err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE $1)`, statement).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case err = <-done:
			t.Fatalf("completion bypassed the expected lock: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("completion did not wait for %q", statement)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
