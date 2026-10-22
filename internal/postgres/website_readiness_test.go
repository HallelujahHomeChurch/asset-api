package postgres

import (
	"context"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
	"strings"
	"testing"
	"time"
)

func TestWebsiteContentReadinessCurrentOwnerBinding(t *testing.T) {
	db := isolatedIntegrationDB(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	insertAsset(t, db, "pdf", assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	if _, err := db.Exec(`UPDATE assets SET namespace='cms.weekly.pdf',owner_service='hhc-web-api',owner_type='bulletin_issue',owner_id='issue',locale='en',visibility='private' WHERE id='pdf'`); err != nil {
		t.Fatal(err)
	}
	c := assets.RecordingCapture{ID: strings.Repeat("a", 32), ActorID: "22222222-2222-4222-8222-222222222222", RecordingID: "11111111-1111-4111-8111-111111111111", CreateKey: "test", State: "uploading", CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}
	if _, err := NewRecordingCaptureStore(db).CreateCapture(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE recording_live SET revision=3,published_revision=2,published_sequence=2,published_media_end=90,last_advanced_at=$1 WHERE capture_id=$2`, now, c.ID); err != nil {
		t.Fatal(err)
	}
	refs := []assets.WebsiteContentRef{{Type: "bulletin_pdf", ID: "pdf", ResourceID: "issue", Locale: "en"}, {Type: "bulletin_pdf", ID: "pdf", ResourceID: "other", Locale: "en"}, {Type: "bulletin_pdf", ID: "pdf", ResourceID: "issue", Locale: "zh-Hant"}, {Type: "bulletin_pdf", ID: "missing", ResourceID: "issue", Locale: "en"}, {Type: "recording_live", ID: c.ID, ResourceID: c.RecordingID}, {Type: "recording_live", ID: c.ID, ResourceID: "wrong"}, {Type: "recording_live", ID: "missing", ResourceID: c.RecordingID}}
	check := func(wantPDF, wantLive bool) {
		t.Helper()
		got, err := New(db).WebsiteContentReadiness(ctx, refs)
		if err != nil || len(got) != len(refs) {
			t.Fatalf("%+v %v", got, err)
		}
		for i, d := range got {
			want := i == 0 && wantPDF || i == 4 && wantLive
			if d.Ref != refs[i] || d.Ready != want {
				t.Fatalf("decision %d %+v", i, d)
			}
			if !want && d.Live != nil {
				t.Fatal("denied live projection")
			}
		}
		if wantLive && (got[4].Live == nil || got[4].Live.Progress.Revision != 2 || got[4].Live.Progress.LastSequence != 2) {
			t.Fatalf("unpublished progress exposed %+v", got[4])
		}
	}
	check(true, true)
	for _, mutation := range []string{`UPDATE assets SET scan_status='infected' WHERE id='pdf'`, `UPDATE assets SET scan_status='clean',deleted_at=now() WHERE id='pdf'`, `UPDATE assets SET deleted_at=NULL,owner_service='account-api' WHERE id='pdf'`} {
		if _, err := db.Exec(mutation); err != nil {
			t.Fatal(err)
		}
		check(false, true)
	}
	if _, err := db.Exec(`UPDATE recording_captures SET terminal_at=now(),state='expired' WHERE id=$1`, c.ID); err != nil {
		t.Fatal(err)
	}
	check(false, false)
	db.Close()
	if _, err := New(db).WebsiteContentReadiness(ctx, refs); err == nil {
		t.Fatal("database outage became deny")
	}
}
