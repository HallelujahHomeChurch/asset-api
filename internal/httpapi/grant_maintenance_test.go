package httpapi

import (
	"context"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/auditclient"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type maintenanceGrantRepository struct {
	*downloadRepository
	source auditclient.Provenance
	called bool
}

func (r *maintenanceGrantRepository) RevokeGrant(ctx context.Context, _, _ string, _ time.Time) error {
	r.called = true
	r.source, _ = auditclient.ProvenanceFromContext(ctx)
	return nil
}
func TestMaintenancePrincipalComesFromAuthenticatedCaller(t *testing.T) {
	for _, tc := range []struct {
		name, caller, token, actor, request string
		status                              int
		service                             bool
	}{
		{"valid", "account-api", "token", "", "cleanup-test", 204, true},
		{"invalid token", "account-api", "wrong", "", "cleanup-test", 403, false},
		{"wrong owner", "hhc-web-api", "token", "", "cleanup-test", 403, false},
		{"invalid actor", "account-api", "token", "forged", "cleanup-test", 204, false},
		{"missing request", "account-api", "token", "", "", 204, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &maintenanceGrantRepository{downloadRepository: &downloadRepository{asset: assets.Asset{ID: "asset-1", OwnerService: "account-api", Namespace: "account.avatar"}}}
			h := New(assets.NewService(repo, nil, "", time.Now), nil, map[string]bool{"account-api": true, "hhc-web-api": true}, false, "token", CallerConfig{}, nil).Routes()
			r := httptest.NewRequest(http.MethodDelete, "/priv/assets/asset-1/grants/grant-1", nil)
			r.Header.Set("Dapr-Caller-App-Id", tc.caller)
			r.Header.Set("dapr-api-token", tc.token)
			r.Header.Set("X-HHC-Request-ID", tc.request)
			r.Header.Set("X-HHC-Actor-ID", tc.actor)
			r.Header.Set("X-HHC-Actor-Type", "service")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || (repo.source.ActorType == "service") != tc.service {
				t.Fatal(w.Code, repo.source)
			}
			if tc.service && repo.source.ActorID != "account-api" {
				t.Fatal("wrong derived executor")
			}
			if tc.status == 403 && repo.called {
				t.Fatal("unauthorized mutation")
			}
		})
	}
}
