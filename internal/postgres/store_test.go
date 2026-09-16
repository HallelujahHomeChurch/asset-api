package postgres

import (
	"errors"
	"testing"

	"hhc/asset-api/internal/assets"
)

func TestAuditUploadMetadataIncludesOnlyCatalogCMSAssets(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	for _, test := range []struct {
		asset   assets.Asset
		audited bool
	}{
		{assets.Asset{ID: id, Namespace: "cms.weekly.pdf", OwnerType: "bulletin_issue", Purpose: "weekly_bulletin"}, true},
		{assets.Asset{ID: id, Namespace: "cms.home.banner", OwnerType: "page", Purpose: "home_banner"}, true},
		{assets.Asset{ID: id, Namespace: "cms.news.cover", OwnerType: "news", Purpose: "news_detail_cover"}, true},
		{assets.Asset{ID: id, Namespace: "line.group.media-sync", OwnerType: "media_sync_ingest"}, false},
		{assets.Asset{ID: id, Namespace: "cms.weekly.pdf", OwnerType: "bulletin_issue", Purpose: "derived"}, false},
	} {
		_, audited := auditUploadMetadata(test.asset)
		if audited != test.audited {
			t.Fatalf("asset=%+v audited=%v", test.asset, audited)
		}
	}
}

func TestFinishRowsReturnsIterationError(t *testing.T) {
	want := errors.New("iteration failed")
	rows := &errorRowSet{err: want}

	err := finishRows(rows)

	if !errors.Is(err, want) || !rows.closed {
		t.Fatalf("err=%v closed=%v", err, rows.closed)
	}
}

type errorRowSet struct {
	err    error
	closed bool
}

func (r *errorRowSet) Err() error {
	return r.err
}

func (r *errorRowSet) Close() error {
	r.closed = true
	return nil
}
