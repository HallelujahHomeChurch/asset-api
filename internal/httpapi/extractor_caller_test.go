package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestVerifiedExtractorUsesOnlyItsBulletinAssetFlows(t *testing.T) {
	const extractor = "hhc-web-extractor"
	fixture := newWorkloadAuthFixture(t)
	h := fixture.handler()
	h.workloadAuth.config.Principals["extractor"] = extractor
	source := assets.Asset{
		ID: "source", OwnerService: "hhc-web-api", OwnerType: "bulletin_issue", OwnerID: "issue",
		Namespace: "cms.weekly.pdf", Purpose: "weekly_bulletin", Locale: "zh-Hant", Visibility: assets.VisibilityPrivate,
		UploadStatus: assets.UploadCompleted, ScanStatus: assets.ScanClean, ProcessingStatus: assets.ProcessingNotRequired,
		DetectedMIMEType: "application/pdf", ETag: `"immutable"`, ObjectKey: "source.pdf", SizeBytes: 6,
	}
	output := assets.Asset{
		ID: "image", OwnerService: source.OwnerService, OwnerType: source.OwnerType, OwnerID: source.OwnerID,
		Namespace: "cms.weekly.image", Purpose: "online_bulletin_illustration", Locale: source.Locale, Visibility: source.Visibility,
		ExpectedMIMEType: "image/png", UploadStatus: assets.UploadCreated, ScanStatus: assets.ScanPending,
	}
	upload := assets.CreateUploadInput{
		OwnerService: source.OwnerService, OwnerType: source.OwnerType, OwnerID: source.OwnerID, Namespace: output.Namespace,
		Purpose: output.Purpose, Locale: output.Locale, Visibility: output.Visibility, ExpectedMIMEType: "image/png",
		OriginalFileName: "image.png", MaxSizeBytes: 1024,
	}
	grant := assets.CreateGrantInput{
		SubjectType: assets.SubjectService, SubjectID: extractor, Permission: assets.PermissionRead,
		IdempotencyKey: "source-read", ExpiresAt: time.Now().Add(30 * time.Minute),
	}
	body := func(input any) string {
		t.Helper()
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	for _, test := range []struct {
		name, method, path, body, subject                   string
		asset                                               assets.Asset
		legacy                                              bool
		want, uploads, sessions, grants, revokes, downloads int
	}{
		{name: "source metadata", method: "GET", path: "/priv/assets/source", asset: source, want: 200},
		{name: "pending illustration metadata", method: "GET", path: "/priv/assets/image", asset: output, want: 200},
		{name: "illustration upload", method: "POST", path: "/priv/assets/upload-sessions", body: body(upload), want: 201, uploads: 1},
		{name: "illustration complete reaches upload authority", method: "POST", path: "/priv/assets/image/complete", asset: output, body: `{}`, want: 404, sessions: 1},
		{name: "own source grant", method: "POST", path: "/priv/assets/source/grants", body: body(grant), asset: source, want: 201, grants: 1},
		{name: "own source download", method: "GET", path: "/priv/assets/source/download", asset: source, subject: extractor, want: 200, downloads: 1},
		{name: "own source grant revoke", method: "DELETE", path: "/priv/assets/source/grants/fixture-grant", asset: source, want: 204, revokes: 1},
		{name: "forged legacy alias", method: "GET", path: "/priv/assets/source", asset: source, legacy: true, want: 403},
		{name: "unrelated operation", method: "GET", path: "/priv/assets/operations", want: 403},
		{name: "source delete", method: "DELETE", path: "/priv/assets/source", asset: source, want: 403},
		{name: "source complete", method: "POST", path: "/priv/assets/source/complete", asset: source, body: `{}`, want: 404},
		{name: "download as CMS", method: "GET", path: "/priv/assets/source/download", asset: source, subject: "hhc-web-api", want: 403},
		{name: "download illustration", method: "GET", path: "/priv/assets/image/download", asset: output, subject: extractor, want: 404},
		{name: "owner illustration alias", method: "GET", path: "/priv/assets/image/bulletin-illustration", asset: output, want: 403},
		{name: "arbitrary CMS metadata", method: "GET", path: "/priv/assets/source", asset: func() assets.Asset { other := source; other.Namespace = "cms.news.cover"; return other }(), want: 404},
		{name: "other locale source", method: "GET", path: "/priv/assets/source", asset: func() assets.Asset { other := source; other.Locale = "en"; return other }(), want: 404},
		{name: "scan pending source download", method: "GET", path: "/priv/assets/source/download", asset: func() assets.Asset { other := source; other.ScanStatus = assets.ScanPending; return other }(), subject: extractor, want: 404},
		{name: "non-extractor upload purpose", method: "POST", path: "/priv/assets/upload-sessions", body: body(func() assets.CreateUploadInput { other := upload; other.Purpose = "arbitrary"; return other }()), want: 403},
		{name: "grant to CMS", method: "POST", path: "/priv/assets/source/grants", body: body(func() assets.CreateGrantInput { other := grant; other.SubjectID = "hhc-web-api"; return other }()), asset: source, want: 403},
		{name: "grant without expiry", method: "POST", path: "/priv/assets/source/grants", body: body(func() assets.CreateGrantInput { other := grant; other.ExpiresAt = time.Time{}; return other }()), asset: source, want: 403},
		{name: "grant exceeds one hour", method: "POST", path: "/priv/assets/source/grants", body: body(func() assets.CreateGrantInput {
			other := grant
			other.ExpiresAt = time.Now().Add(2 * time.Hour)
			return other
		}()), asset: source, want: 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := &extractorRepository{downloadRepository: downloadRepository{asset: test.asset, grantActive: true}}
			blobs := &extractorBlobStore{downloadBlobStore{objects: map[string][]byte{"source.pdf": []byte("%PDF-1")}}}
			h.service = assets.NewService(repository, blobs, "", time.Now)
			r := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			r.Header.Set("X-HHC-Caller-App-Id", extractor)
			r.Header.Set("Idempotency-Key", "fixture")
			if !test.legacy {
				r.Header.Set(workloadHeader, fixture.bearer(t, "first", fixture.firstKey, nil, fixture.claims("extractor")))
			}
			if test.subject != "" {
				r.Header.Set("X-Asset-Subject-Type", "service")
				r.Header.Set("X-Asset-Subject-Id", test.subject)
			}
			w := httptest.NewRecorder()
			h.Routes().ServeHTTP(w, r)
			if w.Code != test.want || repository.uploads != test.uploads || repository.sessions != test.sessions ||
				repository.grants != test.grants || repository.revokes != test.revokes || blobs.openCalls != test.downloads {
				t.Fatalf("status=%d uploads=%d sessions=%d grants=%d revokes=%d downloads=%d body=%s",
					w.Code, repository.uploads, repository.sessions, repository.grants, repository.revokes, blobs.openCalls, w.Body.String())
			}
		})
	}
}

type extractorRepository struct {
	downloadRepository
	uploads, sessions, grants, revokes int
}

func (r *extractorRepository) CreateUpload(context.Context, assets.Asset, assets.UploadSession) error {
	r.uploads++
	return nil
}

func (r *extractorRepository) GetUploadSession(context.Context, string) (assets.UploadSession, error) {
	r.sessions++
	return assets.UploadSession{}, assets.ErrNotFound
}

func (r *extractorRepository) CreateGrant(_ context.Context, grant assets.Grant) (assets.Grant, error) {
	r.grants++
	return grant, nil
}

func (r *extractorRepository) RevokeServiceReadGrant(_ context.Context, _, _, subject string, _ time.Time) error {
	if subject != "hhc-web-extractor" {
		return assets.ErrForbidden
	}
	r.revokes++
	return nil
}

type extractorBlobStore struct{ downloadBlobStore }

func (b *extractorBlobStore) CreateUploadTarget(context.Context, string, int64, time.Time) (assets.UploadTarget, error) {
	return assets.UploadTarget{URL: "https://blob.example/upload", Method: http.MethodPut}, nil
}
