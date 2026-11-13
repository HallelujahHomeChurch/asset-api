package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/auditclient"
	"hhc/asset-api/internal/auditoutbox"
	"hhc/asset-api/internal/migrations"
)

func TestHarnessCollectionReadsWithAuditEnabled(t *testing.T) {
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	config, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = admin.Close() })
	schema := fmt.Sprintf("harness_http_audit_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") })
	config.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = db.Close() })
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	f := newWorkloadAuthFixture(t)
	h := f.handler()
	h.allowedCallers[assets.LineHarnessCaller] = true
	h.workloadAuth.config.Principals["harness"] = assets.LineHarnessCaller
	repository := &collectionManagementRepository{}
	h.service = assets.NewService(repository, &collectionManagementBlobStore{}, "", time.Now)
	h.WithAudit(auditoutbox.New(db))
	for i, path := range []string{"/priv/assets/collections", "/priv/assets/collections/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set(workloadHeader, f.bearer(t, "first", f.firstKey, nil, f.claims("harness")))
		r.Header.Set("X-HHC-Request-ID", fmt.Sprintf("harness-read-%d", i))
		w := httptest.NewRecorder()
		h.Routes().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	rows, err := db.Query("SELECT payload FROM audit_outboxes ORDER BY created_at")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		event, err := auditclient.Decode(payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := event.Canonical(); err != nil {
			t.Fatal(err)
		}
		if event.ActorType != "service" || event.ActorID != assets.LineHarnessCaller {
			t.Fatalf("wrong provenance: %s/%s", event.ActorType, event.ActorID)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("events=%d", count)
	}
	if repository.caller != assets.LineHarnessCaller {
		t.Fatal("caller lost")
	}
}
