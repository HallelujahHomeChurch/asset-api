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

func (p *personalRepository) CreatePersonalFolderGrant(_ context.Context, owner, item, grantee string, _ time.Time) (assets.PersonalFolderGrant, error) {
	p.owner, p.calls = owner, p.calls+1
	return assets.PersonalFolderGrant{ID: "grant", FolderItemID: item, OwnerUserID: owner, GranteeUserID: grantee}, p.err
}
func (p *personalRepository) ListPersonalFolderGrants(_ context.Context, owner, item string) ([]assets.PersonalFolderGrant, error) {
	p.owner, p.calls = owner, p.calls+1
	return []assets.PersonalFolderGrant{{ID: "grant", FolderItemID: item}}, p.err
}
func (p *personalRepository) RevokePersonalFolderGrant(_ context.Context, owner, _, _ string, _ time.Time) error {
	p.owner, p.calls = owner, p.calls+1
	return p.err
}
func (p *personalRepository) ListSharedFolderRoots(_ context.Context, recipient string) ([]assets.SharedFolderRoot, error) {
	p.owner, p.calls = recipient, p.calls+1
	return []assets.SharedFolderRoot{{GrantID: "grant", OwnerUserID: "owner"}}, p.err
}
func (p *personalRepository) SharedFolderSnapshot(_ context.Context, recipient, grant, _ string, _ int) (assets.SharedFolderSnapshot, error) {
	p.owner, p.calls = recipient, p.calls+1
	return assets.SharedFolderSnapshot{GrantID: grant, Reset: true, Items: []assets.PersonalNode{}}, p.err
}
func (p *personalRepository) LeaveSharedFolder(_ context.Context, recipient, _ string, _ time.Time) error {
	p.owner, p.calls = recipient, p.calls+1
	return p.err
}
func (p *personalRepository) SharedFolderContentAssetID(context.Context, string, string, string, time.Time) (string, error) {
	return "asset", p.err
}

func TestPersonalSharingRoutes(t *testing.T) {
	tests := []struct {
		method, path, body string
		status             int
	}{
		{http.MethodPost, "/api/assets/personal-space/items/folder/shares", `{"granteeUserId":"recipient"}`, http.StatusCreated},
		{http.MethodGet, "/api/assets/personal-space/items/folder/shares", "", http.StatusOK},
		{http.MethodDelete, "/api/assets/personal-space/items/folder/shares/grant", "", http.StatusNoContent},
		{http.MethodGet, "/api/assets/shared-folders", "", http.StatusOK},
		{http.MethodGet, "/api/assets/shared-folders/grant/snapshot", "", http.StatusOK},
		{http.MethodDelete, "/api/assets/shared-folders/grant", "", http.StatusNoContent},
	}
	for _, test := range tests {
		repo := &personalRepository{}
		handler := New(assets.NewService(repo, nil, "", time.Now), nil, nil, false, "token", WorkloadAuthConfig{ReaderCallerAppID: "api-gateway"}, nil).Routes()
		request := personalReaderRequest(test.method, test.path)
		request.Body = io.NopCloser(strings.NewReader(test.body))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.status || repo.calls != 1 || repo.owner != "user-acl" {
			t.Fatalf("%s %s status=%d calls=%d owner=%s body=%s", test.method, test.path, response.Code, repo.calls, repo.owner, response.Body.String())
		}
	}
}

func TestSharedFoldersExposeNoMutationRoutes(t *testing.T) {
	repo := &personalRepository{}
	handler := New(assets.NewService(repo, nil, "", time.Now), nil, nil, false, "token", WorkloadAuthConfig{ReaderCallerAppID: "api-gateway"}, nil).Routes()
	for _, path := range []string{"/api/assets/shared-folders/grant/items/item", "/api/assets/shared-folders/grant/items/item/content/upload"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, personalReaderRequest(http.MethodPost, path))
		if response.Code != http.StatusNotFound || repo.calls != 0 {
			t.Fatalf("path=%s status=%d", path, response.Code)
		}
	}
}
