package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

type personalRepository struct {
	assets.Repository
	owner    string
	calls    int
	mutation assets.PersonalMutation
	err      error
}

func (p *personalRepository) EnsurePersonalSpace(_ context.Context, owner string, _ time.Time) (assets.PersonalSpace, error) {
	p.owner = owner
	p.calls++
	return assets.PersonalSpace{ID: "personal", Revision: 1}, p.err
}
func (p *personalRepository) PersonalChanges(_ context.Context, owner, _ string, _ int) (assets.PersonalChangePage, error) {
	p.owner = owner
	p.calls++
	return assets.PersonalChangePage{Items: []assets.PersonalNode{}}, p.err
}
func (p *personalRepository) ApplyPersonalMutation(_ context.Context, owner string, m assets.PersonalMutation, _ time.Time) (assets.PersonalMutationResult, error) {
	p.owner = owner
	p.calls++
	p.mutation = m
	return assets.PersonalMutationResult{ItemID: m.ItemID, NodeRevision: 2, CollectionRevision: 2}, p.err
}

func TestPersonalRoutesRequireTrustedIdentity(t *testing.T) {
	for _, route := range []struct{ method, path, body string }{
		{"POST", "/api/assets/personal-space", ""},
		{"GET", "/api/assets/personal-space/changes", ""},
		{"POST", "/api/assets/personal-space/mutations", `{"operationId":"op","type":"create-folder","itemId":"node","name":"Folder"}`},
	} {
		for _, test := range []struct {
			name   string
			alter  func(*http.Request)
			status int
		}{
			{"trusted", func(*http.Request) {}, 200},
			{"legacy-permission", func(r *http.Request) { r.Header.Set("X-HHC-Scopes", "presenter:cloud:use") }, 200},
			{"missing-permission", func(r *http.Request) { r.Header.Del("X-HHC-Scopes") }, 403},
			{"unrelated-permission", func(r *http.Request) { r.Header.Set("X-HHC-Scopes", "cms:read") }, 403},
			{"forged-user", func(r *http.Request) { r.Header.Del("dapr-api-token") }, 403},
			{"wrong-caller", func(r *http.Request) { r.Header.Set("Dapr-Caller-App-Id", "account-api") }, 403},
			{"missing-user", func(r *http.Request) { r.Header.Del("X-HHC-User-ID") }, 401},
		} {
			t.Run(route.path+"/"+test.name, func(t *testing.T) {
				repo := &personalRepository{}
				handler := New(assets.NewService(repo, nil, "", time.Now), nil, nil, false, "token", WorkloadAuthConfig{ReaderCallerAppID: "api-gateway"}, nil).Routes()
				request := personalReaderRequest(route.method, route.path)
				request.Body = io.NopCloser(strings.NewReader(route.body))
				test.alter(request)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != test.status {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				if test.status == 200 && (repo.calls != 1 || repo.owner != "user-acl") {
					t.Fatalf("owner=%s calls=%d", repo.owner, repo.calls)
				}
				if test.status != 200 && repo.calls != 0 {
					t.Fatalf("unauthorized call=%d", repo.calls)
				}
				if response.Header().Get("Cache-Control") != "private, no-store" {
					t.Fatal("personal response must not be cached")
				}
			})
		}
	}
}

func TestPersonalMutationRejectsOwnerAndDistinguishesPendingScan(t *testing.T) {
	for _, test := range []struct {
		body   string
		err    error
		status int
		code   string
	}{
		{`{"operationId":"op","type":"create-folder","itemId":"node","name":"Folder","owner":"bob"}`, nil, 400, "AST_INVALID_REQUEST"},
		{`{"operationId":"op","type":"rename","itemId":"node","name":"Folder"}`, nil, 400, "AST_INVALID_REQUEST"},
		{`{"operationId":"op","type":"create-file","itemId":"node","name":"file","uploadId":"upload"}`, assets.ErrPersonalAssetNotReady, 409, "asset-not-ready"},
		{`{"operationId":"op","type":"create-folder","itemId":"node","name":"Folder"}`, assets.ErrConflict, 409, "AST_CONFLICT"},
	} {
		repo := &personalRepository{err: test.err}
		handler := New(assets.NewService(repo, nil, "", time.Now), nil, nil, false, "token", WorkloadAuthConfig{ReaderCallerAppID: "api-gateway"}, nil).Routes()
		request := personalReaderRequest("POST", "/api/assets/personal-space/mutations")
		request.Body = io.NopCloser(strings.NewReader(test.body))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.code) {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		if test.status == 400 && repo.calls != 0 {
			t.Fatal("invalid mutation reached store")
		}
	}
}

func personalReaderRequest(method, path string) *http.Request {
	r := collectionReaderRequest(method, path)
	r.Header.Set("X-HHC-Scopes", "openid profile presenter:cloud:manage")
	return r
}

func TestAllPersonalRoutesDenyMissingPermission(t *testing.T) {
	for _, route := range []struct{ method, path string }{
		{"POST", "/api/assets/personal-space"}, {"GET", "/api/assets/personal-space/changes"},
		{"POST", "/api/assets/personal-space/mutations"}, {"POST", "/api/assets/personal-space/uploads"},
		{"GET", "/api/assets/personal-space/uploads/upload"}, {"PUT", "/api/assets/personal-space/uploads/upload/content"},
		{"POST", "/api/assets/personal-space/uploads/upload/complete"}, {"GET", "/api/assets/personal-space/items/item/content"},
	} {
		t.Run(route.path, func(t *testing.T) {
			handler := New(assets.NewService(&personalRepository{}, nil, "", time.Now), nil, nil, false, "token", WorkloadAuthConfig{ReaderCallerAppID: "api-gateway"}, nil).Routes()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, collectionReaderRequest(route.method, route.path))
			if response.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
