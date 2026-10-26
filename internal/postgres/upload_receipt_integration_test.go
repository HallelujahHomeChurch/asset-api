package postgres

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/storage/local"
)

type receiptBlobStore struct {
	*local.Store
	targets atomic.Int32
}

func (b *receiptBlobStore) CreateUploadTarget(ctx context.Context, key string, size int64, expires time.Time) (assets.UploadTarget, error) {
	b.targets.Add(1)
	return b.Store.CreateUploadTarget(ctx, key, size, expires)
}

func TestUploadReplayRetainsUnusableReceiptWithoutSigningOrInserting(t *testing.T) {
	for _, state := range []string{"expired", "purged-expired", "deleted-active", "purged-active", "completed"} {
		t.Run(state, func(t *testing.T) {
			db := integrationDB(t)
			store := New(db)
			ctx := context.Background()
			localStore, err := local.New(t.TempDir(), "http://asset.test/dev/uploads", "0123456789abcdef0123456789abcdef")
			if err != nil {
				t.Fatal(err)
			}
			blobs := &receiptBlobStore{Store: localStore}
			now := time.Now().UTC().Truncate(time.Microsecond)
			svc := assets.NewService(store, blobs, "", func() time.Time { return now })
			input := assets.CreateUploadInput{Namespace: "cms.weekly.image", OwnerService: "hhc-web-api", OwnerType: "bulletin_issue", OwnerID: "issue", Purpose: "online_bulletin_illustration", Locale: "zh-Hant", OriginalFileName: "image.png", ExpectedMIMEType: "image/png", MaxSizeBytes: 100, Visibility: assets.VisibilityPrivate}
			original, err := svc.CreateUploadSession(ctx, input, "source-bound-key")
			if err != nil {
				t.Fatal(err)
			}
			switch state {
			case "expired", "purged-expired":
				now = now.Add(time.Hour)
			case "deleted-active":
				if _, err := db.Exec(`UPDATE assets SET deleted_at=$2 WHERE id=$1`, original.Asset.ID, now); err != nil {
					t.Fatal(err)
				}
			case "completed":
				if _, err := db.Exec(`UPDATE assets SET upload_status='completed',scan_status='clean',detected_mime_type='image/png',size_bytes=100 WHERE id=$1`, original.Asset.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`UPDATE upload_sessions SET status='completed',completed_at=$2 WHERE asset_id=$1`, original.Asset.ID, now); err != nil {
					t.Fatal(err)
				}
			}
			purged := state == "purged-expired" || state == "purged-active"
			if purged {
				if err := store.CompletePurge(ctx, original.Asset.ID, now); err != nil {
					t.Fatal(err)
				}
			}
			replay, err := svc.CreateUploadSession(ctx, input, "source-bound-key")
			if err != nil {
				t.Fatalf("retained receipt replay failed: %v", err)
			}
			if replay.Asset.ID != original.Asset.ID || replay.Session.ID != original.Session.ID || replay.Target.URL != "" || blobs.targets.Load() != 1 {
				t.Fatal("unusable/completed receipt signed or changed identity")
			}
			if state == "deleted-active" || purged {
				if replay.Asset.UploadStatus != assets.UploadFailed || replay.Session.Status != assets.UploadFailed {
					t.Fatal("removed bytes must return an unusable receipt")
				}
			}
			if state == "completed" && replay.Asset.UploadStatus != assets.UploadCompleted {
				t.Fatal("completed replay changed")
			}
			if _, err := store.GetAsset(ctx, original.Asset.ID); purged && !errors.Is(err, assets.ErrNotFound) || !purged && err != nil {
				t.Fatal("ordinary asset visibility changed")
			}
			changed := input
			changed.Purpose = "changed"
			if _, err := svc.CreateUploadSession(ctx, changed, "source-bound-key"); !errors.Is(err, assets.ErrConflict) {
				t.Fatal("changed fingerprint was accepted")
			}
			next, err := svc.CreateUploadSession(ctx, input, "online-image-resume:"+original.Asset.ID)
			if err != nil || next.Asset.ID == original.Asset.ID || next.Target.URL == "" {
				t.Fatalf("stable successor unavailable: %v", err)
			}
			again, err := svc.CreateUploadSession(ctx, input, "online-image-resume:"+original.Asset.ID)
			if err != nil || again.Asset.ID != next.Asset.ID {
				t.Fatal("successor was duplicated")
			}
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM assets`).Scan(&count); err != nil || count != 2 {
				t.Fatalf("rows=%d err=%v", count, err)
			}
		})
	}
}

func TestConcurrentUploadCreationReplaysOneReceipt(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	localStore, err := local.New(t.TempDir(), "http://asset.test/dev/uploads", "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	svc := assets.NewService(store, localStore, "", time.Now)
	input := assets.CreateUploadInput{Namespace: "cms.weekly.image", OwnerService: "hhc-web-api", OwnerType: "bulletin_issue", OwnerID: "issue", Purpose: "online_bulletin_illustration", Locale: "zh-Hant", OriginalFileName: "image.png", ExpectedMIMEType: "image/png", MaxSizeBytes: 100, Visibility: assets.VisibilityPrivate}
	var wg sync.WaitGroup
	ids := make(chan string, 2)
	failures := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		wg.Go(func() {
			<-start
			v, e := svc.CreateUploadSession(context.Background(), input, "concurrent-key")
			if e != nil {
				failures <- e
				return
			}
			if v.Target.URL == "" || v.Session.Status != assets.UploadCreated {
				failures <- errors.New("active upload lost its capability")
				return
			}
			ids <- v.Asset.ID
		})
	}
	close(start)
	wg.Wait()
	close(ids)
	close(failures)
	for e := range failures {
		t.Fatal(e)
	}
	first := ""
	for id := range ids {
		if first != "" && id != first {
			t.Fatal("duplicate asset")
		}
		first = id
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM assets`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows=%d err=%v", count, err)
	}
}
