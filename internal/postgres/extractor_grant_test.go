package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestRevokeServiceReadGrantIsScopedToExactSubjectAndAsset(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	ctx := context.Background()
	now := time.Now().UTC()
	insertAsset(t, db, "source", assets.UploadCompleted, assets.ScanClean, assets.ProcessingNotRequired, now, time.Time{})
	insertAsset(t, db, "other-source", assets.UploadCompleted, assets.ScanClean, assets.ProcessingNotRequired, now, time.Time{})
	for _, grant := range []assets.Grant{
		{ID: "own", SubjectType: assets.SubjectService, SubjectID: "hhc-web-extractor", Permission: assets.PermissionRead},
		{ID: "cms", SubjectType: assets.SubjectService, SubjectID: "hhc-web-api", Permission: assets.PermissionRead},
		{ID: "public", SubjectType: assets.SubjectPublic, SubjectID: "*", Permission: assets.PermissionRead},
		{ID: "delete", SubjectType: assets.SubjectService, SubjectID: "hhc-web-extractor", Permission: assets.PermissionDelete},
	} {
		grant.AssetID, grant.CallerService, grant.Operation = "source", "hhc-web-api", "create_grant"
		grant.IdempotencyKey, grant.CreatedAt = grant.ID, now
		if _, err := store.CreateGrant(ctx, grant); err != nil {
			t.Fatal(err)
		}
	}
	for _, grant := range []string{"cms", "public", "delete"} {
		if err := store.RevokeServiceReadGrant(ctx, "source", grant, "hhc-web-extractor", now); !errors.Is(err, assets.ErrNotFound) {
			t.Fatalf("wrong grant %q error=%v", grant, err)
		}
	}
	if err := store.RevokeServiceReadGrant(ctx, "other-source", "own", "hhc-web-extractor", now); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("cross asset error=%v", err)
	}
	if err := store.RevokeServiceReadGrant(ctx, "source", "own", "hhc-web-extractor", now); err != nil {
		t.Fatal(err)
	}
	var revoked int
	if err := db.QueryRow("SELECT count(*) FROM asset_grants WHERE revoked_at IS NOT NULL").Scan(&revoked); err != nil || revoked != 1 {
		t.Fatalf("revoked=%d err=%v", revoked, err)
	}
	if allowed, err := store.HasActiveGrant(ctx, "source", assets.SubjectService, "hhc-web-extractor", assets.PermissionRead, now); err != nil || allowed {
		t.Fatalf("revoked grant allowed=%v err=%v", allowed, err)
	}
}
