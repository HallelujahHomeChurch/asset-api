package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestCollectionCallerAuthorizationForEveryManagementRoute(t *testing.T) {
	routes := collectionManagementRequests()
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			for _, caller := range []string{"", "api-gateway", "account-api"} {
				t.Run("forbidden-"+caller, func(t *testing.T) {
					handler, _ := newCollectionManagementHandler()
					request := route.request()
					if caller != "" {
						request.Header.Set("X-Internal-Caller-App-Id", caller)
					}
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, request)
					if response.Code != http.StatusForbidden {
						t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
					}
				})
			}

			handler, repository := newCollectionManagementHandler()
			request := route.request()
			request.Header.Set("X-Internal-Caller-App-Id", "hhc-line-function-bot")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != route.wantStatus || repository.calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, repository.calls, response.Body.String())
			}
		})
	}
}

func TestCollectionReaderAuthorizationMatrix(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*http.Request)
		wantStatus int
		wantCalls  int
		wantUser   string
		wantRoles  []string
	}{
		{name: "missing caller", mutate: func(r *http.Request) { r.Header.Del("Dapr-Caller-App-Id") }, wantStatus: http.StatusForbidden},
		{name: "forged caller", mutate: func(r *http.Request) { r.Header.Set("Dapr-Caller-App-Id", "account-api") }, wantStatus: http.StatusForbidden},
		{name: "missing app token", mutate: func(r *http.Request) { r.Header.Del("dapr-api-token") }, wantStatus: http.StatusForbidden},
		{name: "missing user", mutate: func(r *http.Request) { r.Header.Del("X-HHC-User-ID") }, wantStatus: http.StatusUnauthorized},
		{name: "invalid expiry", mutate: func(r *http.Request) { r.Header.Set("X-HHC-Token-Expires-At", "not-unix") }, wantStatus: http.StatusUnauthorized},
		{name: "zero expiry", mutate: func(r *http.Request) { r.Header.Set("X-HHC-Token-Expires-At", "0") }, wantStatus: http.StatusUnauthorized},
		{name: "missing global role", mutate: func(r *http.Request) { r.Header.Set("X-HHC-Roles", "team") }, wantStatus: http.StatusForbidden},
		{name: "user acl", wantStatus: http.StatusOK, wantCalls: 1, wantUser: "user-acl", wantRoles: []string{assets.CollectionReaderRole, "team"}},
		{name: "role acl", mutate: func(r *http.Request) { r.Header.Set("X-HHC-User-ID", "role-user") }, wantStatus: http.StatusOK, wantCalls: 1, wantUser: "role-user", wantRoles: []string{assets.CollectionReaderRole, "team"}},
		{name: "manager only", mutate: func(r *http.Request) {
			r.Header.Set("X-HHC-User-ID", "manager-only")
			r.Header.Set("X-HHC-Roles", assets.CollectionReaderRole+",manager")
		}, wantStatus: http.StatusForbidden, wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, repository := newCollectionReaderHandler()
			request := collectionReaderRequest(http.MethodGet, "/api/assets/collections")
			if test.mutate != nil {
				test.mutate(request)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus || repository.calls != test.wantCalls {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, repository.calls, response.Body.String())
			}
			if test.wantUser != "" && (repository.subject.UserID != test.wantUser || !slices.Equal(repository.subject.Roles, test.wantRoles)) {
				t.Fatalf("subject=%+v", repository.subject)
			}
		})
	}
}

func TestCollectionReaderRoutesUseLiveAuthorization(t *testing.T) {
	tests := []struct {
		name, method, path string
		wantStatus         int
	}{
		{name: "changes", method: http.MethodGet, path: "/api/assets/collections/collection/changes?cursor=next", wantStatus: http.StatusOK},
		{name: "revoked acl", method: http.MethodGet, path: "/api/assets/collections/revoked/changes", wantStatus: http.StatusForbidden},
		{name: "deleted collection", method: http.MethodGet, path: "/api/assets/collections/deleted/changes", wantStatus: http.StatusNotFound},
		{name: "item", method: http.MethodGet, path: "/api/assets/collections/collection/items/item", wantStatus: http.StatusOK},
		{name: "inaccessible item", method: http.MethodGet, path: "/api/assets/collections/collection/items/missing", wantStatus: http.StatusNotFound},
		{name: "item from another collection", method: http.MethodGet, path: "/api/assets/collections/other/items/item", wantStatus: http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, repository := newCollectionReaderHandler()
			request := collectionReaderRequest(test.method, test.path)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus || repository.calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, repository.calls, response.Body.String())
			}
		})
	}
}

func TestCollectionContentTicketIssueRequiresVerifiedGatewayIdentity(t *testing.T) {
	handler, repository, _ := newCollectionContentHandler()
	request := collectionReaderRequest(http.MethodPost, "/api/assets/collections/collection/items/item/content-ticket")
	request.Header.Set("X-HHC-Token-Expires-At", strconv.FormatInt(collectionContentNow.Add(10*time.Minute).Unix(), 10))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var issued assets.ContentTicketResponse
	if err := json.Unmarshal(response.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if issued.ExpiresAt != collectionContentNow.Add(5*time.Minute) || issued.ETag != `"content-version"` {
		t.Fatalf("issued=%+v", issued)
	}
	token := strings.TrimPrefix(issued.ContentURL, "/api/assets/content?ticket=")
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		t.Fatalf("token bytes=%d err=%v", len(raw), err)
	}
	hash := sha256.Sum256(raw)
	if repository.ticket.TokenHash != hex.EncodeToString(hash[:]) || repository.ticket.UserID != "user-acl" || !slices.Equal(repository.ticket.Roles, []string{assets.CollectionReaderRole, "team"}) {
		t.Fatalf("ticket=%+v", repository.ticket)
	}

	for _, test := range []struct {
		name   string
		mutate func(*http.Request)
	}{
		{name: "missing caller", mutate: func(r *http.Request) { r.Header.Del("Dapr-Caller-App-Id") }},
		{name: "forged caller", mutate: func(r *http.Request) { r.Header.Set("Dapr-Caller-App-Id", "account-api") }},
		{name: "malformed expiry", mutate: func(r *http.Request) { r.Header.Set("X-HHC-Token-Expires-At", "invalid") }},
		{name: "expired identity", mutate: func(r *http.Request) {
			r.Header.Set("X-HHC-Token-Expires-At", strconv.FormatInt(collectionContentNow.Unix(), 10))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, repository, _ := newCollectionContentHandler()
			request := collectionReaderRequest(http.MethodPost, "/api/assets/collections/collection/items/item/content-ticket")
			request.Header.Set("X-HHC-Token-Expires-At", strconv.FormatInt(collectionContentNow.Add(time.Minute).Unix(), 10))
			test.mutate(request)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized && response.Code != http.StatusForbidden {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if repository.ticket.TokenHash != "" {
				t.Fatalf("ticket persisted=%+v", repository.ticket)
			}
		})
	}
}

func TestCollectionContentBearerAndTicketConditionalRanges(t *testing.T) {
	validToken := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	paths := []struct {
		name   string
		path   string
		bearer bool
	}{
		{name: "bearer", path: "/api/assets/collections/collection/items/item/content", bearer: true},
		{name: "ticket", path: "/api/assets/content?ticket=" + validToken + "&remoteItemId=ignored&filename=evil.txt"},
	}
	tests := []struct {
		name, method, rangeValue, ifNoneMatch, ifRange string
		status                                         int
		body, contentRange                             string
		openCalls                                      int
	}{
		{name: "full", method: http.MethodGet, status: http.StatusOK, body: "abcdef", openCalls: 1},
		{name: "head", method: http.MethodHead, status: http.StatusOK},
		{name: "not modified", method: http.MethodGet, ifNoneMatch: `"content-version"`, status: http.StatusNotModified},
		{name: "single range", method: http.MethodGet, rangeValue: "bytes=1-3", status: http.StatusPartialContent, body: "bcd", contentRange: "bytes 1-3/6", openCalls: 1},
		{name: "suffix range", method: http.MethodGet, rangeValue: "bytes=-2", status: http.StatusPartialContent, body: "ef", contentRange: "bytes 4-5/6", openCalls: 1},
		{name: "unsatisfied", method: http.MethodGet, rangeValue: "bytes=99-100", status: http.StatusRequestedRangeNotSatisfiable, contentRange: "bytes */6"},
		{name: "resumed video", method: http.MethodGet, rangeValue: "bytes=2-", ifRange: `"content-version"`, status: http.StatusPartialContent, body: "cdef", contentRange: "bytes 2-5/6", openCalls: 1},
		{name: "stale resume validator", method: http.MethodGet, rangeValue: "bytes=2-", ifRange: `"old-version"`, status: http.StatusOK, body: "abcdef", openCalls: 1},
	}
	for _, path := range paths {
		for _, test := range tests {
			t.Run(path.name+"/"+test.name, func(t *testing.T) {
				handler, _, blobs := newCollectionContentHandler()
				request := httptest.NewRequest(test.method, path.path, nil)
				request.Header.Set("Dapr-Caller-App-Id", "api-gateway")
				request.Header.Set("dapr-api-token", "token")
				if path.bearer {
					for name, values := range collectionReaderRequest(test.method, path.path).Header {
						request.Header[name] = values
					}
					request.Header.Set("X-HHC-Token-Expires-At", strconv.FormatInt(collectionContentNow.Add(time.Minute).Unix(), 10))
				} else {
					request.Header.Set("Authorization", "Bearer forged")
					request.Header.Set("X-HHC-User-ID", "attacker")
					request.Header.Set("X-HHC-Roles", "admin")
				}
				request.Header.Set("Range", test.rangeValue)
				request.Header.Set("If-None-Match", test.ifNoneMatch)
				request.Header.Set("If-Range", test.ifRange)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != test.status || (test.status != http.StatusRequestedRangeNotSatisfiable && response.Body.String() != test.body) || blobs.openCalls != test.openCalls || response.Header().Get("Content-Range") != test.contentRange {
					t.Fatalf("status=%d body=%q open=%d range=%q", response.Code, response.Body.String(), blobs.openCalls, response.Header().Get("Content-Range"))
				}
				for name, value := range map[string]string{"Cache-Control": "private, no-store", "Referrer-Policy": "no-referrer", "Accept-Ranges": "bytes", "ETag": `"content-version"`} {
					if response.Header().Get(name) != value {
						t.Fatalf("%s=%q", name, response.Header().Get(name))
					}
				}
				if path.name == "ticket" && test.name == "full" {
					disposition := response.Header().Get("Content-Disposition")
					if !strings.Contains(disposition, "video.mp4") || strings.Contains(disposition, "evil.txt") {
						t.Fatalf("content disposition=%q", disposition)
					}
				}
			})
		}
	}
}

func TestCollectionTicketRouteRequiresExactAuthenticatedGatewayCaller(t *testing.T) {
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))
	for _, test := range []struct {
		name   string
		mutate func(*http.Request)
	}{
		{name: "missing caller", mutate: func(r *http.Request) { r.Header.Del("Dapr-Caller-App-Id") }},
		{name: "forged caller", mutate: func(r *http.Request) { r.Header.Set("Dapr-Caller-App-Id", "account-api") }},
		{name: "missing app token", mutate: func(r *http.Request) { r.Header.Del("dapr-api-token") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, repository, blobs := newCollectionContentHandler()
			request := httptest.NewRequest(http.MethodGet, "/api/assets/content?ticket="+token, nil)
			request.Header.Set("Dapr-Caller-App-Id", "api-gateway")
			request.Header.Set("dapr-api-token", "token")
			request.Header.Set("X-HHC-User-ID", "forged-user")
			test.mutate(request)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || repository.ticketHash != "" || blobs.openCalls != 0 {
				t.Fatalf("status=%d lookup=%q opens=%d body=%s", response.Code, repository.ticketHash, blobs.openCalls, response.Body.String())
			}
		})
	}
}

func TestCollectionTicketMiddlewareClearsExternalIdentityAndNonTicketQuery(t *testing.T) {
	handler := &Handler{appAPIToken: "token", workloadAuth: WorkloadAuthConfig{ReaderCallerAppID: "api-gateway"}}
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if ticket, _ := r.Context().Value(contentTicketKey{}).(string); ticket != "opaque-ticket" {
			t.Fatalf("ticket=%q", ticket)
		}
		if r.URL.RawQuery != "" || r.RequestURI != "/api/assets/content" {
			t.Fatalf("query=%q requestURI=%q", r.URL.RawQuery, r.RequestURI)
		}
		for _, name := range []string{"Authorization", "Cookie", "X-HHC-User-ID", "X-HHC-Roles", "X-MS-CLIENT-PRINCIPAL", "X-Asset-Subject-Id", "X-Internal-Caller-App-Id", "Dapr-Caller-App-Id", "dapr-api-token"} {
			if r.Header.Get(name) != "" {
				t.Fatalf("%s was forwarded", name)
			}
		}
		if r.Header.Get("Range") != "bytes=1-" || r.Header.Get("If-Range") != `"etag"` {
			t.Fatalf("range headers=%v", r.Header)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodGet, "/api/assets/content?ticket=opaque-ticket&remoteItemId=ignore", nil)
	request.Header.Set("Dapr-Caller-App-Id", "api-gateway")
	request.Header.Set("dapr-api-token", "token")
	request.Header.Set("Authorization", "Bearer forged")
	request.Header.Set("Cookie", "session=forged")
	request.Header.Set("X-HHC-User-ID", "forged")
	request.Header.Set("X-HHC-Roles", "admin")
	request.Header.Set("X-MS-CLIENT-PRINCIPAL", "forged")
	request.Header.Set("X-Asset-Subject-Id", "forged")
	request.Header.Set("X-Internal-Caller-App-Id", "forged")
	request.Header.Set("Range", "bytes=1-")
	request.Header.Set("If-Range", `"etag"`)
	response := httptest.NewRecorder()
	handler.collectionTicket(next).ServeHTTP(response, request)
	if !called || response.Code != http.StatusNoContent {
		t.Fatalf("called=%v status=%d", called, response.Code)
	}
}

func TestCollectionTicketRouteRejectsMissingInvalidAndRevokedWithoutTelemetryLeak(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, ticket := range []string{"", "secret-invalid-ticket"} {
		handler, repository, blobs := newCollectionContentHandler()
		repository.rejectTicket = true
		request := httptest.NewRequest(http.MethodGet, "/api/assets/content?ticket="+ticket+"&remoteItemId=must-ignore", nil)
		request.Header.Set("Dapr-Caller-App-Id", "api-gateway")
		request.Header.Set("dapr-api-token", "token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized || blobs.openCalls != 0 || !strings.Contains(response.Body.String(), "AST_INVALID_TICKET") {
			t.Fatalf("ticket=%q status=%d opens=%d body=%s", ticket, response.Code, blobs.openCalls, response.Body.String())
		}
		if ticket != "" && (strings.Contains(response.Body.String(), ticket) || strings.Contains(logs.String(), ticket)) {
			t.Fatalf("ticket leaked body=%s logs=%s", response.Body.String(), logs.String())
		}
	}
}

var collectionContentNow = time.Date(2026, 8, 16, 8, 0, 0, 0, time.UTC)

func newCollectionContentHandler() (http.Handler, *collectionContentRepository, *downloadBlobStore) {
	repository := &collectionContentRepository{asset: assets.Asset{
		ID: "asset", ObjectKey: "content", OriginalFileName: "video.mp4", DetectedMIMEType: "video/mp4",
		SizeBytes: 6, ETag: `"content-version"`, UploadStatus: assets.UploadCompleted, ScanStatus: assets.ScanClean,
		ProcessingStatus: assets.ProcessingReady, UpdatedAt: collectionContentNow,
	}}
	blobs := &downloadBlobStore{objects: map[string][]byte{"content": []byte("abcdef")}}
	service := assets.NewService(repository, blobs, "", func() time.Time { return collectionContentNow })
	handler := New(service, nil, nil, false, "token", WorkloadAuthConfig{ReaderCallerAppID: "api-gateway"}, nil).Routes()
	return handler, repository, blobs
}

type collectionContentRepository struct {
	assets.Repository
	asset          assets.Asset
	ticket         assets.ContentTicket
	ticketHash     string
	rejectTicket   bool
	readerItemCall int
}

func (r *collectionContentRepository) GetAuthorizedCollectionItem(_ context.Context, collectionID, itemID string, subject assets.CollectionSubject) (assets.CollectionItem, error) {
	r.readerItemCall++
	if collectionID != "collection" || itemID != "item" {
		return assets.CollectionItem{}, assets.ErrNotFound
	}
	if !readerTestAuthorized(subject) {
		return assets.CollectionItem{}, assets.ErrForbidden
	}
	return assets.CollectionItem{ID: itemID, CollectionID: collectionID, AssetID: r.asset.ID, ETag: r.asset.ETag}, nil
}

func (r *collectionContentRepository) GetAsset(_ context.Context, id string) (assets.Asset, error) {
	if id != r.asset.ID {
		return assets.Asset{}, assets.ErrNotFound
	}
	return r.asset, nil
}

func (r *collectionContentRepository) CreateContentTicket(_ context.Context, ticket assets.ContentTicket, _ time.Time) error {
	r.ticket = ticket
	return nil
}

func (r *collectionContentRepository) RedeemContentTicket(_ context.Context, tokenHash string, _ time.Time) (assets.Asset, error) {
	r.ticketHash = tokenHash
	if r.rejectTicket {
		return assets.Asset{}, assets.ErrNotFound
	}
	return r.asset, nil
}

func TestCollectionReaderMetadataIsSyncSafe(t *testing.T) {
	handler, _ := newCollectionReaderHandler()
	for _, path := range []string{
		"/api/assets/collections/collection/changes",
		"/api/assets/collections/collection/items/item",
	} {
		request := collectionReaderRequest(http.MethodGet, path)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("path=%s status=%d body=%s", path, response.Code, response.Body.String())
		}
		body := response.Body.String()
		for _, secret := range []string{`"assetId"`, "secret-asset", "blob-key", "owner-service", "line-group-id", "ticket-hash"} {
			if strings.Contains(body, secret) {
				t.Fatalf("path=%s leaked %q in %s", path, secret, body)
			}
		}
		for _, metadata := range []string{`"id":"item"`, `"remoteItemId":"remote-item"`, `"displayName":"Media"`, `"sourceRevision":"source"`, `"mimeType":"video/mp4"`, `"sizeBytes":20`, `"etag":"etag"`} {
			if !strings.Contains(body, metadata) {
				t.Fatalf("path=%s missing %s in %s", path, metadata, body)
			}
		}
	}
}

func TestCollectionReaderAcceptsPositivePastUnixExpiry(t *testing.T) {
	handler, repository := newCollectionReaderHandler()
	request := collectionReaderRequest(http.MethodGet, "/api/assets/collections")
	request.Header.Set("X-HHC-Token-Expires-At", "1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || repository.calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, repository.calls, response.Body.String())
	}
}

func collectionReaderRequest(method, path string) *http.Request {
	request := httptest.NewRequest(method, path, nil)
	request.Header.Set("Dapr-Caller-App-Id", "api-gateway")
	request.Header.Set("dapr-api-token", "token")
	request.Header.Set("X-HHC-User-ID", "user-acl")
	request.Header.Set("X-HHC-Roles", " "+assets.CollectionReaderRole+", team, "+assets.CollectionReaderRole+" ")
	request.Header.Set("X-HHC-Token-ID", "token-id")
	request.Header.Set("X-HHC-Token-Expires-At", "1")
	request.Header.Set("X-HHC-Session-ID", "session-id")
	request.Header.Set("X-HHC-Auth-Provider", "account-api")
	return request
}

func newCollectionReaderHandler() (http.Handler, *collectionReaderRepository) {
	repository := &collectionReaderRepository{}
	service := assets.NewService(repository, &collectionManagementBlobStore{}, "", func() time.Time { return time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC) })
	return New(service, nil, map[string]bool{"hhc-line-function-bot": true}, false, "token", WorkloadAuthConfig{ReaderCallerAppID: "api-gateway"}, nil).Routes(), repository
}

type collectionReaderRepository struct {
	assets.Repository
	calls   int
	subject assets.CollectionSubject
}

func (r *collectionReaderRepository) ListAuthorizedCollections(_ context.Context, subject assets.CollectionSubject, _ string, _ int) (assets.CollectionPage, error) {
	r.calls++
	r.subject = subject
	if !readerTestAuthorized(subject) {
		return assets.CollectionPage{}, assets.ErrForbidden
	}
	return assets.CollectionPage{Collections: []assets.Collection{{ID: "collection", Namespace: "line.group.media-sync", Name: "Media", Revision: 2}}}, nil
}

func (r *collectionReaderRepository) CollectionChanges(_ context.Context, id, cursor string, subject assets.CollectionSubject) (assets.CollectionChangePage, error) {
	r.calls++
	r.subject = subject
	if id == "deleted" {
		return assets.CollectionChangePage{}, assets.ErrNotFound
	}
	if id == "revoked" || !readerTestAuthorized(subject) {
		return assets.CollectionChangePage{}, assets.ErrForbidden
	}
	return assets.CollectionChangePage{
		Collection: assets.Collection{ID: id, Revision: 2}, Cursor: cursor,
		Items:      []assets.CollectionItem{{ID: "item", CollectionID: id, AssetID: "secret-asset", RemoteItemID: "remote-item", DisplayName: "Media", SourceRevision: "source", CreatedRevision: 2, MIMEType: "video/mp4", SizeBytes: 20, ETag: "etag"}},
		Tombstones: []assets.CollectionTombstone{},
	}, nil
}

func (r *collectionReaderRepository) GetAuthorizedCollectionItem(_ context.Context, collectionID, itemID string, subject assets.CollectionSubject) (assets.CollectionItem, error) {
	r.calls++
	r.subject = subject
	if !readerTestAuthorized(subject) {
		return assets.CollectionItem{}, assets.ErrForbidden
	}
	if collectionID != "collection" || itemID != "item" {
		return assets.CollectionItem{}, assets.ErrNotFound
	}
	return assets.CollectionItem{ID: itemID, CollectionID: collectionID, AssetID: "secret-asset", RemoteItemID: "remote-item", DisplayName: "Media", SourceRevision: "source", CreatedRevision: 2, MIMEType: "video/mp4", SizeBytes: 20, ETag: "etag"}, nil
}

func readerTestAuthorized(subject assets.CollectionSubject) bool {
	if subject.UserID == "user-acl" || subject.UserID == "role-user" {
		return slices.Contains(subject.Roles, assets.CollectionReaderRole) && slices.Contains(subject.Roles, "team")
	}
	return false
}

func TestCollectionManagementMutationsRequireHeaderIdempotency(t *testing.T) {
	for _, route := range collectionManagementRequests() {
		if route.method == http.MethodGet {
			continue
		}
		for _, key := range []string{"", "   "} {
			t.Run(route.name+"-"+key, func(t *testing.T) {
				handler, repository := newCollectionManagementHandler()
				request := route.request()
				request.Header.Set("X-Internal-Caller-App-Id", "hhc-line-function-bot")
				request.Header.Set("Idempotency-Key", key)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != http.StatusBadRequest || repository.calls != 0 {
					t.Fatalf("status=%d calls=%d body=%s", response.Code, repository.calls, response.Body.String())
				}
			})
		}
	}
}

func TestCollectionManagementBodyCannotSetTrustedIdentity(t *testing.T) {
	for _, field := range []string{`"idempotencyKey":"body-key"`, `"callerService":"account-api"`} {
		handler, repository := newCollectionManagementHandler()
		request := httptest.NewRequest(http.MethodPost, "/priv/assets/collections", bytes.NewBufferString(`{"namespace":"line.group.media-sync","name":"Media",`+field+`}`))
		request.Header.Set("X-Internal-Caller-App-Id", "hhc-line-function-bot")
		request.Header.Set("Idempotency-Key", "header-key")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || repository.calls != 0 {
			t.Fatalf("field=%s status=%d calls=%d body=%s", field, response.Code, repository.calls, response.Body.String())
		}
	}
}

func TestCollectionManagementValidationAndManagedReads(t *testing.T) {
	tests := []struct {
		name, method, path, body, key string
	}{
		{name: "blank name", method: http.MethodPost, path: "/priv/assets/collections", body: `{"namespace":"line.group.media-sync","name":"   "}`, key: "key"},
		{name: "long name", method: http.MethodPatch, path: "/priv/assets/collections/collection", body: `{"name":"` + strings.Repeat("a", 121) + `"}`, key: "key"},
		{name: "long display name", method: http.MethodPost, path: "/priv/assets/collections/collection/items", body: `{"assetId":"asset","remoteItemId":"remote","displayName":"` + strings.Repeat("a", 256) + `","sourceRevision":"source"}`, key: "key"},
		{name: "blank remote item", method: http.MethodPost, path: "/priv/assets/collections/collection/items", body: `{"assetId":"asset","remoteItemId":"","displayName":"Media","sourceRevision":"source"}`, key: "key"},
		{name: "long remote item", method: http.MethodPost, path: "/priv/assets/collections/collection/items", body: `{"assetId":"asset","remoteItemId":"` + strings.Repeat("a", 256) + `","displayName":"Media","sourceRevision":"source"}`, key: "key"},
		{name: "long idempotency", method: http.MethodDelete, path: "/priv/assets/collections/collection", key: strings.Repeat("a", 129)},
		{name: "long opaque collection id", method: http.MethodGet, path: "/priv/assets/collections/" + strings.Repeat("a", 256)},
		{name: "unknown field", method: http.MethodPost, path: "/priv/assets/collections", body: `{"namespace":"line.group.media-sync","name":"Media","unknown":true}`, key: "key"},
		{name: "trailing value", method: http.MethodPost, path: "/priv/assets/collections", body: `{"namespace":"line.group.media-sync","name":"Media"} {}`, key: "key"},
		{name: "invalid limit", method: http.MethodGet, path: "/priv/assets/collections?limit=nope"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, repository := newCollectionManagementHandler()
			request := httptest.NewRequest(test.method, test.path, bytes.NewBufferString(test.body))
			request.Header.Set("X-Internal-Caller-App-Id", "hhc-line-function-bot")
			request.Header.Set("Idempotency-Key", test.key)
			request.Header.Set("X-HHC-Request-ID", "request-1")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || repository.calls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, repository.calls, response.Body.String())
			}
			if response.Header().Get("X-HHC-Request-ID") != "request-1" {
				t.Fatalf("request ID=%q", response.Header().Get("X-HHC-Request-ID"))
			}
		})
	}

	handler, repository := newCollectionManagementHandler()
	list := httptest.NewRequest(http.MethodGet, "/priv/assets/collections?cursor=next&limit=25", nil)
	list.Header.Set("X-Internal-Caller-App-Id", "hhc-line-function-bot")
	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, list)
	get := httptest.NewRequest(http.MethodGet, "/priv/assets/collections/collection", nil)
	get.Header.Set("X-Internal-Caller-App-Id", "hhc-line-function-bot")
	getResponse := httptest.NewRecorder()
	handler.ServeHTTP(getResponse, get)
	if listResponse.Code != http.StatusOK || getResponse.Code != http.StatusOK || repository.managedListCalls != 1 || repository.managedGetCalls != 1 || repository.readerCalls != 0 {
		t.Fatalf("list=%d get=%d managedList=%d managedGet=%d reader=%d", listResponse.Code, getResponse.Code, repository.managedListCalls, repository.managedGetCalls, repository.readerCalls)
	}
	if repository.listCursor != "next" || repository.listLimit != 25 || repository.caller != "hhc-line-function-bot" {
		t.Fatalf("cursor=%q limit=%d caller=%q", repository.listCursor, repository.listLimit, repository.caller)
	}
}

func TestCollectionManagementTrimsNamesAndUsesAuthenticatedCaller(t *testing.T) {
	handler, repository := newCollectionManagementHandler()
	request := httptest.NewRequest(http.MethodPost, "/priv/assets/collections", bytes.NewBufferString(`{"namespace":"line.group.media-sync","name":"  Media  "}`))
	request.Header.Set("X-Internal-Caller-App-Id", "hhc-line-function-bot")
	request.Header.Set("Idempotency-Key", "  request-key  ")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || repository.create.Name != "Media" || repository.create.CallerService != "hhc-line-function-bot" || repository.create.IdempotencyKey != "request-key" {
		t.Fatalf("status=%d input=%+v body=%s", response.Code, repository.create, response.Body.String())
	}
}

func TestCollectionManagementAcceptsInclusiveByteLimits(t *testing.T) {
	for _, test := range []struct {
		name, path, body, key string
	}{
		{name: "collection", path: "/priv/assets/collections", body: `{"namespace":"line.group.media-sync","name":"` + strings.Repeat("a", 120) + `"}`, key: strings.Repeat("k", 128)},
		{name: "item", path: "/priv/assets/collections/collection/items", body: `{"assetId":"asset","remoteItemId":"` + strings.Repeat("r", 255) + `","displayName":"` + strings.Repeat("d", 255) + `","sourceRevision":"source"}`, key: "key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, repository := newCollectionManagementHandler()
			request := httptest.NewRequest(http.MethodPost, test.path, bytes.NewBufferString(test.body))
			request.Header.Set("X-Internal-Caller-App-Id", "hhc-line-function-bot")
			request.Header.Set("Idempotency-Key", test.key)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusCreated || repository.calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, repository.calls, response.Body.String())
			}
		})
	}
}

func TestPrivateAssetActionRejectsUnknownAction(t *testing.T) {
	handler, _ := newCollectionManagementHandler()
	request := httptest.NewRequest(http.MethodGet, "/priv/assets/asset/unknown", nil)
	request.Header.Set("X-Internal-Caller-App-Id", "hhc-line-function-bot")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

type collectionManagementRequest struct {
	name, method string
	wantStatus   int
	request      func() *http.Request
}

func collectionManagementRequests() []collectionManagementRequest {
	request := func(method, path, body string, mutation bool) func() *http.Request {
		return func() *http.Request {
			value := httptest.NewRequest(method, path, bytes.NewBufferString(body))
			if mutation {
				value.Header.Set("Idempotency-Key", "key")
			}
			return value
		}
	}
	return []collectionManagementRequest{
		{name: "list", method: http.MethodGet, wantStatus: http.StatusOK, request: request(http.MethodGet, "/priv/assets/collections", "", false)},
		{name: "get", method: http.MethodGet, wantStatus: http.StatusOK, request: request(http.MethodGet, "/priv/assets/collections/collection", "", false)},
		{name: "create", method: http.MethodPost, wantStatus: http.StatusCreated, request: request(http.MethodPost, "/priv/assets/collections", `{"namespace":"line.group.media-sync","name":"Media"}`, true)},
		{name: "rename", method: http.MethodPatch, wantStatus: http.StatusOK, request: request(http.MethodPatch, "/priv/assets/collections/collection", `{"name":"Renamed"}`, true)},
		{name: "delete", method: http.MethodDelete, wantStatus: http.StatusOK, request: request(http.MethodDelete, "/priv/assets/collections/collection", "", true)},
		{name: "add acl", method: http.MethodPost, wantStatus: http.StatusCreated, request: request(http.MethodPost, "/priv/assets/collections/collection/acl", `{"subjectType":"user","subjectId":"user","permission":"read"}`, true)},
		{name: "revoke acl", method: http.MethodDelete, wantStatus: http.StatusOK, request: request(http.MethodDelete, "/priv/assets/collections/collection/acl/acl", "", true)},
		{name: "add item", method: http.MethodPost, wantStatus: http.StatusCreated, request: request(http.MethodPost, "/priv/assets/collections/collection/items", `{"assetId":"asset","remoteItemId":"remote","displayName":"Media","sourceRevision":"source"}`, true)},
		{name: "delete item", method: http.MethodDelete, wantStatus: http.StatusOK, request: request(http.MethodDelete, "/priv/assets/collections/collection/items/item", "", true)},
	}
}

func newCollectionManagementHandler() (http.Handler, *collectionManagementRepository) {
	repository := &collectionManagementRepository{}
	service := assets.NewService(repository, &collectionManagementBlobStore{}, "", func() time.Time { return time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC) })
	handler := New(service, nil, map[string]bool{"hhc-line-function-bot": true, "api-gateway": true, "account-api": true}, true, "", WorkloadAuthConfig{}, nil).Routes()
	return handler, repository
}

type collectionManagementRepository struct {
	assets.Repository
	calls, managedListCalls, managedGetCalls, readerCalls int
	caller, listCursor                                    string
	listLimit                                             int
	create                                                assets.CreateCollectionInput
}

func (r *collectionManagementRepository) GetAsset(context.Context, string) (assets.Asset, error) {
	return assets.Asset{}, assets.ErrNotFound
}

func (r *collectionManagementRepository) CreateCollection(_ context.Context, input assets.CreateCollectionInput, _ time.Time) (assets.Collection, error) {
	r.calls++
	r.create = input
	return assets.Collection{ID: "collection", Namespace: input.Namespace, Name: input.Name}, nil
}
func (r *collectionManagementRepository) RenameCollection(_ context.Context, input assets.RenameCollectionInput, _ time.Time) (assets.Collection, error) {
	r.calls++
	return assets.Collection{ID: input.CollectionID, Name: input.Name}, nil
}
func (r *collectionManagementRepository) DeleteCollection(_ context.Context, input assets.DeleteCollectionInput, _ time.Time) (assets.Collection, error) {
	r.calls++
	return assets.Collection{ID: input.CollectionID}, nil
}
func (r *collectionManagementRepository) AddCollectionACL(_ context.Context, input assets.AddCollectionACLInput, _ time.Time) (assets.CollectionACLMutation, error) {
	r.calls++
	return assets.CollectionACLMutation{Collection: assets.Collection{ID: input.CollectionID}, ACL: assets.CollectionACL{ID: "acl"}}, nil
}
func (r *collectionManagementRepository) RevokeCollectionACL(_ context.Context, input assets.RevokeCollectionACLInput, _ time.Time) (assets.CollectionACLMutation, error) {
	r.calls++
	return assets.CollectionACLMutation{Collection: assets.Collection{ID: input.CollectionID}, ACL: assets.CollectionACL{ID: input.ACLID}}, nil
}
func (r *collectionManagementRepository) AddCollectionItem(_ context.Context, input assets.AddCollectionItemInput, _ time.Time) (assets.CollectionItemMutation, error) {
	r.calls++
	return assets.CollectionItemMutation{Collection: assets.Collection{ID: input.CollectionID}, Item: assets.CollectionItem{ID: "item"}}, nil
}
func (r *collectionManagementRepository) DeleteCollectionItem(_ context.Context, input assets.DeleteCollectionItemInput, _ time.Time) (assets.CollectionItemMutation, error) {
	r.calls++
	return assets.CollectionItemMutation{Collection: assets.Collection{ID: input.CollectionID}, Item: assets.CollectionItem{ID: input.ItemID}}, nil
}
func (r *collectionManagementRepository) ListManagedCollections(_ context.Context, caller, cursor string, limit int) (assets.ManagedCollectionPage, error) {
	r.calls++
	r.managedListCalls++
	r.caller, r.listCursor, r.listLimit = caller, cursor, limit
	return assets.ManagedCollectionPage{Collections: []assets.ManagedCollection{}}, nil
}
func (r *collectionManagementRepository) GetManagedCollection(_ context.Context, id, caller string) (assets.ManagedCollection, error) {
	r.calls++
	r.managedGetCalls++
	r.caller = caller
	return assets.ManagedCollection{Collection: assets.Collection{ID: id}}, nil
}
func (r *collectionManagementRepository) ListAuthorizedCollections(context.Context, assets.CollectionSubject, string, int) (assets.CollectionPage, error) {
	r.readerCalls++
	return assets.CollectionPage{}, errors.New("reader path must not be used")
}

type collectionManagementBlobStore struct{ assets.BlobStore }

func TestParseRange(t *testing.T) {
	value, partial, err := parseRange("bytes=10-19")
	if err != nil || !partial || value.Offset != 10 || value.Count != 10 {
		t.Fatalf("range=%+v partial=%v err=%v", value, partial, err)
	}
	value, partial, err = parseRange("bytes=10-")
	if err != nil || !partial || value.Offset != 10 || value.Count != 0 {
		t.Fatalf("open range=%+v partial=%v err=%v", value, partial, err)
	}
}

func TestParseRangeAcceptsSuffix(t *testing.T) {
	value, partial, err := parseRange("bytes=-10")
	if err != nil || !partial || value.Suffix != 10 {
		t.Fatalf("range=%+v partial=%v err=%v", value, partial, err)
	}
}

func TestParseRangeRejectsInvalidSuffixAndMultipleRanges(t *testing.T) {
	for _, input := range []string{"bytes=-0", "bytes=-9223372036854775808", "bytes=0-1,3-4", "items=0-1"} {
		if _, _, err := parseRange(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}

func TestProductionCallerUsesDaprIdentity(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/priv/assets/upload-sessions", nil)
	request.Header.Set("Dapr-Caller-App-Id", "account-api")
	request.Header.Set("X-Internal-Caller-App-Id", "hhc-web-api")

	if caller := callerFromRequest(request, false); caller != "account-api" {
		t.Fatalf("caller = %q", caller)
	}
}

func TestProductionCallerRejectsDevelopmentHeader(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/priv/assets/upload-sessions", nil)
	request.Header.Set("X-Internal-Caller-App-Id", "account-api")

	if caller := callerFromRequest(request, false); caller != "" {
		t.Fatalf("caller = %q", caller)
	}
	if caller := callerFromRequest(request, true); caller != "account-api" {
		t.Fatalf("development caller = %q", caller)
	}
}

func TestInternalRequiresAppTokenInProduction(t *testing.T) {
	handler := (&Handler{
		allowedCallers: map[string]bool{"account-api": true},
		appAPIToken:    "secret",
	}).internal(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, test := range []struct {
		name  string
		token string
		want  int
	}{
		{name: "missing", want: http.StatusForbidden},
		{name: "wrong", token: "wrong", want: http.StatusForbidden},
		{name: "valid", token: "secret", want: http.StatusNoContent},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/priv/assets/upload-sessions", nil)
			request.Header.Set("Dapr-Caller-App-Id", "account-api")
			request.Header.Set("dapr-api-token", test.token)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestInternalAllowsExplicitDevelopmentCaller(t *testing.T) {
	handler := (&Handler{
		allowedCallers:       map[string]bool{"account-api": true},
		allowDevCallerHeader: true,
	}).internal(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "/priv/assets/upload-sessions", nil)
	request.Header.Set("X-Internal-Caller-App-Id", "account-api")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestInternalAllowsValidatedEntraWorkload(t *testing.T) {
	auth := WorkloadAuthConfig{
		TenantID: "tenant-1", Audience: "api://asset-api", Issuer: "https://login.microsoftonline.com/tenant-1/v2.0",
		RequiredRole: "Asset.Invoke", Callers: map[string]WorkloadCaller{
			"line-client": {ObjectID: "line-object", Service: "hhc-line-function-bot"},
		},
	}
	handler := (&Handler{allowedCallers: map[string]bool{"hhc-line-function-bot": true}, workloadAuth: auth}).internal(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if callerFromRequest(r, false) != "hhc-line-function-bot" {
			t.Fatalf("caller = %q", callerFromRequest(r, false))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "/priv/assets/upload-sessions", nil)
	request.Header.Set("X-MS-CLIENT-PRINCIPAL", encodedPrincipal(t, map[string]string{
		"tid": "tenant-1", "iss": auth.Issuer, "aud": auth.Audience, "appid": "line-client", "oid": "line-object", "roles": auth.RequiredRole,
	}))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestManagedIdentityV1IssuerIsLimitedToConfiguredTenant(t *testing.T) {
	config := WorkloadAuthConfig{
		TenantID: "tenant-1", Audience: "api://asset-api", Issuer: "https://login.microsoftonline.com/tenant-1/v2.0",
		RequiredRole: "Asset.Invoke", Callers: map[string]WorkloadCaller{"line-client": {ObjectID: "line-object", Service: "hhc-line-function-bot"}},
	}
	claims := map[string]string{
		"tid": "tenant-1", "iss": "https://sts.windows.net/tenant-1/", "aud": config.Audience,
		"appid": "line-client", "oid": "line-object", "roles": config.RequiredRole,
	}
	if caller := workloadCaller(encodedPrincipal(t, claims), config); caller != "hhc-line-function-bot" {
		t.Fatalf("caller = %q", caller)
	}
	claims["iss"] = "https://sts.windows.net/other-tenant/"
	if caller := workloadCaller(encodedPrincipal(t, claims), config); caller != "" {
		t.Fatalf("cross-tenant caller = %q", caller)
	}
}

func TestInternalRejectsInvalidEntraWorkloadClaims(t *testing.T) {
	auth := WorkloadAuthConfig{
		TenantID: "tenant-1", Audience: "api://asset-api", Issuer: "https://login.microsoftonline.com/tenant-1/v2.0", RequiredRole: "Asset.Invoke",
		Callers: map[string]WorkloadCaller{"line-client": {ObjectID: "line-object", Service: "hhc-line-function-bot"}},
	}
	base := map[string]string{"tid": auth.TenantID, "iss": auth.Issuer, "aud": auth.Audience, "appid": "line-client", "oid": "line-object", "roles": auth.RequiredRole}
	for _, claim := range []string{"tid", "iss", "aud", "appid", "oid", "roles"} {
		t.Run(claim, func(t *testing.T) {
			claims := maps.Clone(base)
			claims[claim] = "wrong"
			handler := (&Handler{allowedCallers: map[string]bool{"hhc-line-function-bot": true}, workloadAuth: auth}).internal(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
			request := httptest.NewRequest(http.MethodPost, "/priv/assets/upload-sessions", nil)
			request.Header.Set("X-MS-CLIENT-PRINCIPAL", encodedPrincipal(t, claims))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d", response.Code)
			}
		})
	}
}

func encodedPrincipal(t *testing.T, claims map[string]string) string {
	t.Helper()
	value := struct {
		AuthType string `json:"auth_typ"`
		Claims   []struct {
			Type  string `json:"typ"`
			Value string `json:"val"`
		} `json:"claims"`
	}{AuthType: "aad"}
	for key, claim := range claims {
		value.Claims = append(value.Claims, struct {
			Type  string `json:"typ"`
			Value string `json:"val"`
		}{Type: key, Value: claim})
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(encoded)
}

func TestLocalUploadCORSAllowsAdminDevelopmentOrigin(t *testing.T) {
	nextCalled := false
	handler := localUploadCORS(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodOptions, "/dev/uploads/token", nil)
	request.Header.Set("Origin", "http://127.0.0.1:5175")
	request.Header.Set("Access-Control-Request-Method", http.MethodPut)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent || nextCalled {
		t.Fatalf("status=%d next=%v", response.Code, nextCalled)
	}
	if response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5175" {
		t.Fatalf("allow origin=%q", response.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestPublicDownloadHeadMatchesGetWithoutOpeningBlob(t *testing.T) {
	for _, variant := range []string{"", "small"} {
		t.Run(variant, func(t *testing.T) {
			headHandler, headBlobs := publicDownloadHandler(t)
			path := "/api/assets/public/asset-1"
			if variant != "" {
				path += "/" + variant
			}
			head := httptest.NewRecorder()
			headHandler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, path, nil))

			getHandler, _ := publicDownloadHandler(t)
			get := httptest.NewRecorder()
			getHandler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, path, nil))

			if head.Code != get.Code || head.Code != http.StatusOK {
				t.Fatalf("HEAD=%d GET=%d", head.Code, get.Code)
			}
			for _, name := range []string{"Content-Type", "Content-Length", "ETag", "Last-Modified", "Cache-Control", "Accept-Ranges"} {
				if head.Header().Get(name) != get.Header().Get(name) {
					t.Fatalf("%s: HEAD=%q GET=%q", name, head.Header().Get(name), get.Header().Get(name))
				}
			}
			if head.Body.Len() != 0 || headBlobs.openCalls != 0 {
				t.Fatalf("HEAD body=%q openCalls=%d", head.Body.String(), headBlobs.openCalls)
			}
		})
	}
}

func TestPublicDownloadPreservesOriginalFileName(t *testing.T) {
	handler, _ := publicDownloadHandler(t)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/assets/public/asset-1", nil))

	disposition, params, err := mime.ParseMediaType(response.Header().Get("Content-Disposition"))
	if err != nil || disposition != "inline" || params["filename"] != "更新1732期週報.pdf" {
		t.Fatalf("Content-Disposition=%q parsed=%q params=%v err=%v", response.Header().Get("Content-Disposition"), disposition, params, err)
	}
}

func TestPublicDownloadAcceptsSafeFileNameOverride(t *testing.T) {
	handler, _ := publicDownloadHandler(t)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/assets/public/asset-1?filename=1732-%E6%9C%AC%E9%80%B1%E9%80%B1%E5%A0%B1.pdf", nil))

	_, params, err := mime.ParseMediaType(response.Header().Get("Content-Disposition"))
	if err != nil || params["filename"] != "1732-本週週報.pdf" {
		t.Fatalf("Content-Disposition=%q params=%v err=%v", response.Header().Get("Content-Disposition"), params, err)
	}
}

func TestPublicDownloadRejectsUnsafeFileNameOverride(t *testing.T) {
	handler, _ := publicDownloadHandler(t)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/assets/public/asset-1?filename=weekly.exe", nil))

	_, params, err := mime.ParseMediaType(response.Header().Get("Content-Disposition"))
	if err != nil || params["filename"] != "更新1732期週報.pdf" {
		t.Fatalf("Content-Disposition=%q params=%v err=%v", response.Header().Get("Content-Disposition"), params, err)
	}
}

func TestPublicDownloadNotModifiedDoesNotOpenBlob(t *testing.T) {
	for _, test := range []struct {
		path  string
		value string
	}{
		{path: "/api/assets/public/asset-1", value: `W/"other", W/"original"`},
		{path: "/api/assets/public/asset-1/small", value: `W/"small"`},
		{path: "/api/assets/public/asset-1", value: "*"},
	} {
		handler, blobs := publicDownloadHandler(t)
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		request.Header.Set("If-None-Match", test.value)
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		if response.Code != http.StatusNotModified || response.Body.Len() != 0 || blobs.openCalls != 0 {
			t.Fatalf("If-None-Match=%q status=%d body=%q openCalls=%d", test.value, response.Code, response.Body.String(), blobs.openCalls)
		}
	}
}

func TestPublicDownloadRangeUsesStoredTotal(t *testing.T) {
	handler, blobs := publicDownloadHandler(t)
	request := httptest.NewRequest(http.MethodGet, "/api/assets/public/asset-1", nil)
	request.Header.Set("Range", "bytes=1-99")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusPartialContent || response.Header().Get("Content-Range") != "bytes 1-5/6" || response.Header().Get("Content-Length") != "5" || response.Body.String() != "bcdef" || blobs.openCalls != 1 {
		t.Fatalf("status=%d headers=%v body=%q openCalls=%d", response.Code, response.Header(), response.Body.String(), blobs.openCalls)
	}
}

func TestPublicDownloadSuffixRange(t *testing.T) {
	for _, test := range []struct {
		value        string
		contentRange string
		body         string
	}{
		{value: "bytes=-2", contentRange: "bytes 4-5/6", body: "ef"},
		{value: "bytes=-99", contentRange: "bytes 0-5/6", body: "abcdef"},
	} {
		handler, blobs := publicDownloadHandler(t)
		request := httptest.NewRequest(http.MethodGet, "/api/assets/public/asset-1", nil)
		request.Header.Set("Range", test.value)
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		if response.Code != http.StatusPartialContent || response.Header().Get("Content-Range") != test.contentRange || response.Body.String() != test.body || blobs.openCalls != 1 {
			t.Fatalf("range=%q status=%d content-range=%q body=%q openCalls=%d", test.value, response.Code, response.Header().Get("Content-Range"), response.Body.String(), blobs.openCalls)
		}
	}
}

func TestPublicDownloadIfRangeControlsPartialResponse(t *testing.T) {
	modified := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		validator string
		status    int
		body      string
	}{
		{name: "matching etag", validator: `"original"`, status: http.StatusPartialContent, body: "bc"},
		{name: "mismatched etag", validator: `"other"`, status: http.StatusOK, body: "abcdef"},
		{name: "weak etag", validator: `W/"original"`, status: http.StatusOK, body: "abcdef"},
		{name: "matching date", validator: modified.Format(http.TimeFormat), status: http.StatusPartialContent, body: "bc"},
		{name: "older date", validator: modified.Add(-time.Second).Format(http.TimeFormat), status: http.StatusOK, body: "abcdef"},
		{name: "invalid date", validator: "not-a-validator", status: http.StatusOK, body: "abcdef"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, blobs := publicDownloadHandler(t)
			request := httptest.NewRequest(http.MethodGet, "/api/assets/public/asset-1", nil)
			request.Header.Set("Range", "bytes=1-2")
			request.Header.Set("If-Range", test.validator)
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != test.status || response.Body.String() != test.body || blobs.openCalls != 1 {
				t.Fatalf("status=%d body=%q openCalls=%d", response.Code, response.Body.String(), blobs.openCalls)
			}
			if test.status == http.StatusOK && response.Header().Get("Content-Range") != "" {
				t.Fatalf("full response has Content-Range %q", response.Header().Get("Content-Range"))
			}
		})
	}
}

func TestPublicDownloadSuffixRangeRejectsEmptyRepresentation(t *testing.T) {
	handler, blobs := publicDownloadHandlerWithOriginal(t, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/assets/public/asset-1", nil)
	request.Header.Set("Range", "bytes=-1")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusRequestedRangeNotSatisfiable || response.Header().Get("Content-Range") != "bytes */0" || blobs.openCalls != 0 {
		t.Fatalf("status=%d content-range=%q openCalls=%d", response.Code, response.Header().Get("Content-Range"), blobs.openCalls)
	}
}

func TestPublicDownloadUnsatisfiedRangeDoesNotOpenBlob(t *testing.T) {
	for _, value := range []string{"bytes=6-", "bytes=-0", "bytes=-9223372036854775808", "bytes=0-1,3-4", "bytes=0-9223372036854775807"} {
		handler, blobs := publicDownloadHandler(t)
		request := httptest.NewRequest(http.MethodGet, "/api/assets/public/asset-1", nil)
		request.Header.Set("Range", value)
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		if response.Code != http.StatusRequestedRangeNotSatisfiable || response.Header().Get("Content-Range") != "bytes */6" || blobs.openCalls != 0 {
			t.Fatalf("range=%q status=%d content-range=%q openCalls=%d", value, response.Code, response.Header().Get("Content-Range"), blobs.openCalls)
		}
	}
}

func TestAssetErrorsAreNotCacheable(t *testing.T) {
	handler, _ := publicDownloadHandler(t)
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/assets/public/missing", nil),
		httptest.NewRequest(http.MethodGet, "/api/assets/public/asset-1", nil),
	} {
		if request.URL.Path != "/api/assets/public/missing" {
			request.Header.Set("Range", "bytes=99-100")
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code < 400 || response.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("path=%s status=%d cache=%q", request.URL.Path, response.Code, response.Header().Get("Cache-Control"))
		}
	}
}

func TestRestrictedDownloadRequiresOwnerAndSubjectGrant(t *testing.T) {
	modified := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	repository := &downloadRepository{
		asset: assets.Asset{
			ID: "asset-1", Namespace: "line.group.file", OwnerService: "hhc-line-function-bot",
			ObjectKey: "original", DetectedMIMEType: "application/pdf", SizeBytes: 6, ETag: `"original"`,
			UploadStatus: assets.UploadCompleted, ScanStatus: assets.ScanClean,
			ProcessingStatus: assets.ProcessingNotRequired, Visibility: assets.VisibilityRestricted, UpdatedAt: modified,
		},
	}
	blobs := &downloadBlobStore{objects: map[string][]byte{"original": []byte("secret")}}
	service := assets.NewService(repository, blobs, "https://www.alive.org.tw/api/assets", func() time.Time { return modified })
	handler := New(service, nil, map[string]bool{"hhc-line-function-bot": true}, false, "token", WorkloadAuthConfig{}, nil).Routes()

	request := httptest.NewRequest(http.MethodGet, "/priv/assets/asset-1/download", nil)
	request.Header.Set("Dapr-Caller-App-Id", "hhc-line-function-bot")
	request.Header.Set("dapr-api-token", "token")
	request.Header.Set("X-Asset-Subject-Type", "line_group")
	request.Header.Set("X-Asset-Subject-Id", "group-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "secret" {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("cache control=%q", response.Header().Get("Cache-Control"))
	}
}

func publicDownloadHandler(t *testing.T) (http.Handler, *downloadBlobStore) {
	t.Helper()
	return publicDownloadHandlerWithOriginal(t, []byte("abcdef"))
}

func publicDownloadHandlerWithOriginal(t *testing.T, original []byte) (http.Handler, *downloadBlobStore) {
	t.Helper()
	modified := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	repository := &downloadRepository{
		asset: assets.Asset{
			ID: "asset-1", Namespace: "cms.news.cover", ObjectKey: "original", DetectedMIMEType: "image/jpeg",
			OriginalFileName: "更新1732期週報.pdf",
			SizeBytes:        int64(len(original)), ETag: `"original"`, UploadStatus: assets.UploadCompleted, ScanStatus: assets.ScanClean,
			ProcessingStatus: assets.ProcessingReady, Visibility: assets.VisibilityPublic, UpdatedAt: modified,
		},
		derivative: assets.Derivative{
			AssetID: "asset-1", Variant: "small", ObjectKey: "small", MIMEType: "image/jpeg",
			SizeBytes: 3, ETag: `"small"`, CreatedAt: modified,
		},
	}
	blobs := &downloadBlobStore{objects: map[string][]byte{"original": original, "small": []byte("xyz")}}
	service := assets.NewService(repository, blobs, "https://www.alive.org.tw/api/assets", func() time.Time { return modified })
	return New(service, nil, nil, false, "", WorkloadAuthConfig{}, nil).Routes(), blobs
}

type downloadRepository struct {
	assets.Repository
	asset      assets.Asset
	derivative assets.Derivative
}

func (r *downloadRepository) GetAsset(_ context.Context, id string) (assets.Asset, error) {
	if id != r.asset.ID {
		return assets.Asset{}, assets.ErrNotFound
	}
	return r.asset, nil
}
func (r *downloadRepository) HasActiveGrant(context.Context, string, assets.SubjectType, string, assets.Permission, time.Time) (bool, error) {
	return true, nil
}
func (r *downloadRepository) GetDerivative(_ context.Context, assetID, variant string) (assets.Derivative, error) {
	if assetID != r.derivative.AssetID || variant != r.derivative.Variant {
		return assets.Derivative{}, assets.ErrNotFound
	}
	return r.derivative, nil
}

type downloadBlobStore struct {
	assets.BlobStore
	objects   map[string][]byte
	openCalls int
}

func (b *downloadBlobStore) Open(_ context.Context, key string, byteRange assets.ByteRange, _ string) (assets.BlobDownload, error) {
	b.openCalls++
	value, ok := b.objects[key]
	if !ok {
		return assets.BlobDownload{}, assets.ErrNotFound
	}
	if byteRange.Offset > 0 || byteRange.Count > 0 {
		value = value[byteRange.Offset : byteRange.Offset+byteRange.Count]
	}
	return assets.BlobDownload{Body: io.NopCloser(bytes.NewReader(value)), Size: int64(len(value))}, nil
}
