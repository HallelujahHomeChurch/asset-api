package postgres

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/auditclient"
	"hhc/asset-api/internal/auditoutbox"
	"hhc/asset-api/internal/httpapi"
)

func TestPublicGrantRevocationProvenanceTransaction(t *testing.T) {
	for _, tc := range []struct {
		name, actorType, actor, owner, namespace, subject string
		fail                                              bool
	}{
		{name: "missing", owner: "hhc-web-api", namespace: "cms.news.cover", subject: "public", fail: true},
		{name: "user", actorType: "user", actor: testAuditActorUserID, owner: "hhc-web-api", namespace: "cms.news.cover", subject: "public"},
		{name: "maintenance", actorType: "service", actor: "account-api", owner: "account-api", namespace: "account.avatar", subject: "public"},
		{name: "wrong service", actorType: "service", actor: "hhc-web-api", owner: "hhc-web-api", namespace: "cms.news.cover", subject: "public", fail: true},
		{name: "wrong owner", actorType: "service", actor: "account-api", owner: "hhc-web-api", namespace: "account.avatar", subject: "public", fail: true},
		{name: "wrong namespace", actorType: "service", actor: "account-api", owner: "account-api", namespace: "cms.news.cover", subject: "public", fail: true},
		{name: "private", owner: "hhc-web-api", namespace: "cms.news.cover", subject: "user"},
		{name: "invalid actor", actorType: "user", actor: "invalid", owner: "hhc-web-api", namespace: "cms.news.cover", subject: "public", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := integrationDB(t)
			store := New(db)
			store.WithAudit(auditoutbox.New(db))
			now := time.Now().UTC()
			const id = "0123456789abcdef0123456789abcdef"
			insertAsset(t, db, id, assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
			if _, err := db.Exec(`UPDATE assets SET owner_service=$2,namespace=$3 WHERE id=$1`, id, tc.owner, tc.namespace); err != nil {
				t.Fatal(err)
			}
			grant, err := store.CreateGrant(context.Background(), assets.Grant{ID: "grant-1", AssetID: id, SubjectType: assets.SubjectType(tc.subject), SubjectID: "*", Permission: assets.PermissionRead, IdempotencyKey: "grant-1", CallerService: tc.owner, Operation: "create_grant", CreatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if tc.actorType != "" {
				ctx = auditclient.WithProvenance(ctx, tc.actorType, tc.actor, "revoke-test")
			}
			err = store.RevokeGrant(ctx, id, grant.ID, now)
			if tc.fail {
				if err == nil {
					t.Fatal("unsafe revoke succeeded")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			var revoked sql.NullTime
			var count int
			if err := db.QueryRow(`SELECT revoked_at FROM asset_grants WHERE id=$1`, grant.ID).Scan(&revoked); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM audit_outboxes`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if !tc.fail && tc.subject == "public" {
				want = 1
			}
			if revoked.Valid == tc.fail || count != want {
				t.Fatalf("revoked=%v events=%d", revoked.Valid, count)
			}
			if !tc.fail {
				if err := store.RevokeGrant(ctx, id, grant.ID, now); !errors.Is(err, assets.ErrNotFound) {
					t.Fatalf("retry=%v", err)
				}
				if err := db.QueryRow(`SELECT count(*) FROM audit_outboxes`).Scan(&count); err != nil || count != want {
					t.Fatal("duplicate audit", count, err)
				}
			}
		})
	}
}

func TestPublicGrantHTTPMissingSourceRollsBack(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	store.WithAudit(auditoutbox.New(db))
	now := time.Now().UTC()
	const id = "0123456789abcdef0123456789abcdef"
	insertAsset(t, db, id, assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	if _, err := db.Exec(`UPDATE assets SET owner_service='hhc-web-api' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	_, err := store.CreateGrant(context.Background(), assets.Grant{ID: "grant-1", AssetID: id, SubjectType: assets.SubjectPublic, SubjectID: "*", Permission: assets.PermissionRead, IdempotencyKey: "grant-1", CallerService: "hhc-web-api", Operation: "create_grant", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	h := httpapi.New(assets.NewService(store, nil, "", time.Now), nil, map[string]bool{"hhc-web-api": true, "account-api": true}, false, "token", httpapi.CallerConfig{}, nil).Routes()
	for _, tc := range []struct {
		actor, caller string
		code          int
	}{{"", "hhc-web-api", 503}, {testAuditActorUserID, "account-api", 403}, {testAuditActorUserID, "hhc-web-api", 204}, {testAuditActorUserID, "hhc-web-api", 404}} {
		r := httptest.NewRequest(http.MethodDelete, "/priv/assets/"+id+"/grants/grant-1", nil)
		r.Header.Set("Dapr-Caller-App-Id", tc.caller)
		r.Header.Set("dapr-api-token", "token")
		r.Header.Set("X-HHC-Request-ID", "revoke-test")
		r.Header.Set("X-HHC-Actor-ID", tc.actor)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("status=%d want=%d", w.Code, tc.code)
		}
	}
}

func TestPublicGrantAuditEnqueueFailureRollsBack(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	store.WithAudit(auditoutbox.New(db))
	now := time.Now().UTC()
	const id = "0123456789abcdef0123456789abcdef"
	insertAsset(t, db, id, assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	_, err := store.CreateGrant(context.Background(), assets.Grant{ID: "grant-1", AssetID: id, SubjectType: assets.SubjectPublic, SubjectID: "*", Permission: assets.PermissionRead, IdempotencyKey: "grant-1", CallerService: "test", Operation: "create_grant", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE FUNCTION reject_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic failure'; END $$; CREATE TRIGGER reject_audit BEFORE INSERT ON audit_outboxes FOR EACH ROW EXECUTE FUNCTION reject_audit()`); err != nil {
		t.Fatal(err)
	}
	ctx := auditclient.WithProvenance(context.Background(), "user", testAuditActorUserID, "enqueue-test")
	if err := store.RevokeGrant(ctx, id, "grant-1", now); !errors.Is(err, assets.ErrAuditUnavailable) {
		t.Fatal(err)
	}
	var revoked sql.NullTime
	if err := db.QueryRow(`SELECT revoked_at FROM asset_grants WHERE id='grant-1'`).Scan(&revoked); err != nil || revoked.Valid {
		t.Fatal("grant escaped rollback", err)
	}
}

func TestUnsupportedServiceRevocationPreservesAuditUnavailable(t *testing.T) {
	db := integrationDB(t)
	store := New(db)
	store.WithAudit(auditoutbox.New(db))
	now := time.Now().UTC()
	const id = "0123456789abcdef0123456789abcdef"
	insertAsset(t, db, id, assets.UploadCompleted, assets.ScanClean, assets.ProcessingReady, now, time.Time{})
	_, err := store.CreateGrant(context.Background(), assets.Grant{ID: "grant-1", AssetID: id, SubjectType: assets.SubjectPublic, SubjectID: "*", Permission: assets.PermissionRead, IdempotencyKey: "grant-1", CallerService: "test", Operation: "create_grant", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	ctx := auditclient.WithProvenance(context.Background(), "service", "hhc-line-function-bot", "service-revoke-test")
	if err := store.RevokeGrant(ctx, id, "grant-1", now); !errors.Is(err, assets.ErrAuditUnavailable) {
		t.Fatalf("existing audit failure contract changed: %v", err)
	}
}
