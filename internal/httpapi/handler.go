package httpapi

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"hhc/asset-api/internal/assets"
)

type Handler struct {
	service              *assets.Service
	db                   *sql.DB
	allowedCallers       map[string]bool
	allowDevCallerHeader bool
	appAPIToken          string
	workloadAuth         WorkloadAuthConfig
	localUpload          http.HandlerFunc
}

type WorkloadCaller struct {
	ObjectID string
	Service  string
}

type WorkloadAuthConfig struct {
	TenantID          string
	Issuer            string
	Audience          string
	RequiredRole      string
	ReaderCallerAppID string
	Callers           map[string]WorkloadCaller
}

func New(service *assets.Service, db *sql.DB, allowedCallers map[string]bool, allowDevCallerHeader bool, appAPIToken string, workloadAuth WorkloadAuthConfig, localUpload http.HandlerFunc) *Handler {
	return &Handler{service: service, db: db, allowedCallers: allowedCallers, allowDevCallerHeader: allowDevCallerHeader, appAPIToken: appAPIToken, workloadAuth: workloadAuth, localUpload: localUpload}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "healthy"})
	})
	mux.HandleFunc("GET /ready", h.ready)
	mux.HandleFunc("GET /api/assets/public/{assetID}", h.publicDownload)
	mux.HandleFunc("GET /api/assets/public/{assetID}/{variant}", h.publicDerivativeDownload)
	mux.Handle("GET /api/assets/collections", h.collectionReader(http.HandlerFunc(h.listAuthorizedCollections)))
	mux.Handle("GET /api/assets/collections/{collectionID}/changes", h.collectionReader(http.HandlerFunc(h.collectionChanges)))
	mux.Handle("GET /api/assets/collections/{collectionID}/items/{itemID}", h.collectionReader(http.HandlerFunc(h.getAuthorizedCollectionItem)))
	mux.Handle("POST /api/assets/collections/{collectionID}/items/{itemID}/content-ticket", h.collectionReader(http.HandlerFunc(h.collectionContentPending)))
	mux.Handle("GET /api/assets/collections/{collectionID}/items/{itemID}/content", h.collectionReader(http.HandlerFunc(h.collectionContentPending)))
	mux.Handle("POST /priv/assets/upload-sessions", h.internal(http.HandlerFunc(h.createUpload)))
	mux.Handle("GET /priv/assets/operations", h.internal(http.HandlerFunc(h.operations)))
	mux.Handle("GET /priv/assets/collections", h.internal(h.collectionCaller(http.HandlerFunc(h.listManagedCollections))))
	mux.Handle("GET /priv/assets/collections/{collectionID}", h.internal(h.collectionCaller(http.HandlerFunc(h.getManagedCollection))))
	mux.Handle("POST /priv/assets/collections", h.internal(h.collectionCaller(http.HandlerFunc(h.createCollection))))
	mux.Handle("PATCH /priv/assets/collections/{collectionID}", h.internal(h.collectionCaller(http.HandlerFunc(h.renameCollection))))
	mux.Handle("DELETE /priv/assets/collections/{collectionID}", h.internal(h.collectionCaller(http.HandlerFunc(h.deleteCollection))))
	mux.Handle("POST /priv/assets/collections/{collectionID}/acl", h.internal(h.collectionCaller(http.HandlerFunc(h.addCollectionACL))))
	mux.Handle("DELETE /priv/assets/collections/{collectionID}/acl/{aclID}", h.internal(h.collectionCaller(http.HandlerFunc(h.revokeCollectionACL))))
	mux.Handle("POST /priv/assets/collections/{collectionID}/items", h.internal(h.collectionCaller(http.HandlerFunc(h.addCollectionItem))))
	mux.Handle("DELETE /priv/assets/collections/{collectionID}/items/{itemID}", h.internal(h.collectionCaller(http.HandlerFunc(h.deleteCollectionItem))))
	mux.Handle("GET /priv/assets/{assetID}", h.internal(http.HandlerFunc(h.getAsset)))
	mux.Handle("GET /priv/assets/{assetID}/{action}", h.internal(http.HandlerFunc(h.assetAction)))
	mux.Handle("POST /priv/assets/{assetID}/complete", h.internal(http.HandlerFunc(h.completeUpload)))
	mux.Handle("POST /priv/assets/{assetID}/grants", h.internal(http.HandlerFunc(h.createGrant)))
	mux.Handle("DELETE /priv/assets/{assetID}/grants/{grantID}", h.internal(http.HandlerFunc(h.revokeGrant)))
	mux.Handle("POST /priv/assets/{assetID}/scan/requeue", h.internal(http.HandlerFunc(h.requeueScan)))
	mux.Handle("DELETE /priv/assets/{assetID}", h.internal(http.HandlerFunc(h.deleteAsset)))
	if h.localUpload != nil {
		mux.Handle("/dev/uploads/{token}", localUploadCORS(http.HandlerFunc(h.localUpload)))
	}
	return requestID(mux)
}

func (h *Handler) assetAction(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("action") {
	case "download":
		h.authorizedDownload(w, r)
	case "public-url":
		h.publicURL(w, r)
	default:
		http.NotFound(w, r)
	}
}

func localUploadCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "http://127.0.0.1:5175" || origin == "http://localhost:5175" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "PUT, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length")
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) internal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller := ""
		if h.allowDevCallerHeader {
			caller = strings.TrimSpace(r.Header.Get("X-Internal-Caller-App-Id"))
		} else if daprCaller := h.daprCaller(r); daprCaller != "" {
			caller = daprCaller
		} else {
			caller = workloadCaller(r.Header.Get("X-MS-CLIENT-PRINCIPAL"), h.workloadAuth)
		}
		if !h.allowedCallers[caller] {
			writeError(w, http.StatusForbidden, "AST_FORBIDDEN", "caller is not allowed")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerContextKey{}, caller)))
	})
}

func (h *Handler) collectionReader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.daprCaller(r) != h.workloadAuth.ReaderCallerAppID || h.workloadAuth.ReaderCallerAppID == "" {
			writeError(w, http.StatusForbidden, "AST_FORBIDDEN", "caller is not allowed")
			return
		}
		identity, ok := parseCollectionReaderIdentity(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "AST_UNAUTHORIZED", "authenticated user identity is required")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), collectionReaderIdentityKey{}, identity)))
	})
}

func (h *Handler) daprCaller(r *http.Request) string {
	if !sameToken(r.Header.Get("dapr-api-token"), h.appAPIToken) {
		return ""
	}
	return strings.TrimSpace(r.Header.Get("Dapr-Caller-App-Id"))
}

func (h *Handler) collectionCaller(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authenticatedCaller(r) != "hhc-line-function-bot" {
			writeError(w, http.StatusForbidden, "AST_FORBIDDEN", "caller is not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sameToken(got, want string) bool {
	return got != "" && want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.db.PingContext(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "AST_NOT_READY", "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (h *Handler) createUpload(w http.ResponseWriter, r *http.Request) {
	var input assets.CreateUploadInput
	if !decodeJSON(w, r, &input) {
		return
	}
	caller := callerFromRequest(r, h.allowDevCallerHeader)
	policy, ok := assets.PolicyFor(input.Namespace)
	if !ok || input.OwnerService != caller || policy.OwnerService != caller {
		writeError(w, http.StatusForbidden, "AST_FORBIDDEN", "caller cannot use this asset namespace")
		return
	}
	created, err := h.service.CreateUploadSession(r.Context(), input, r.Header.Get("Idempotency-Key"))
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}
func (h *Handler) getAsset(w http.ResponseWriter, r *http.Request) {
	asset, err := h.service.GetAsset(r.Context(), r.PathValue("assetID"))
	if err != nil {
		handleError(w, err)
		return
	}
	if asset.OwnerService != callerFromRequest(r, h.allowDevCallerHeader) {
		writeError(w, http.StatusForbidden, "AST_FORBIDDEN", "caller does not own this asset")
		return
	}
	writeJSON(w, http.StatusOK, asset)
}
func (h *Handler) completeUpload(w http.ResponseWriter, r *http.Request) {
	if !h.requireOwnedAsset(w, r) {
		return
	}
	var input assets.CompleteUploadInput
	if !decodeJSON(w, r, &input) {
		return
	}
	asset, err := h.service.CompleteUpload(r.Context(), r.PathValue("assetID"), input)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, asset)
}
func (h *Handler) createGrant(w http.ResponseWriter, r *http.Request) {
	if !h.requireOwnedAsset(w, r) {
		return
	}
	var input assets.CreateGrantInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.IdempotencyKey == "" {
		input.IdempotencyKey = r.Header.Get("Idempotency-Key")
	}
	grant, err := h.service.CreateGrant(r.Context(), r.PathValue("assetID"), input)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, grant)
}
func (h *Handler) revokeGrant(w http.ResponseWriter, r *http.Request) {
	if !h.requireOwnedAsset(w, r) {
		return
	}
	if err := h.service.RevokeGrant(r.Context(), r.PathValue("assetID"), r.PathValue("grantID")); err != nil {
		handleError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (h *Handler) publicURL(w http.ResponseWriter, r *http.Request) {
	if !h.requireOwnedAsset(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"assetId": r.PathValue("assetID"), "downloadUrl": h.service.PublicURL(r.PathValue("assetID"))})
}

func (h *Handler) authorizedDownload(w http.ResponseWriter, r *http.Request) {
	if !h.requireOwnedAsset(w, r) {
		return
	}
	metadata, err := h.service.AuthorizedMetadata(
		r.Context(),
		r.PathValue("assetID"),
		assets.SubjectType(r.Header.Get("X-Asset-Subject-Type")),
		r.Header.Get("X-Asset-Subject-Id"),
	)
	if err != nil {
		handleError(w, err)
		return
	}
	h.serveDownload(w, r, metadata)
}

func (h *Handler) deleteAsset(w http.ResponseWriter, r *http.Request) {
	if err := h.service.SoftDelete(r.Context(), r.PathValue("assetID"), callerFromRequest(r, h.allowDevCallerHeader)); err != nil {
		handleError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) requeueScan(w http.ResponseWriter, r *http.Request) {
	if !h.requireOwnedAsset(w, r) {
		return
	}
	if err := h.service.RequeueScan(r.Context(), r.PathValue("assetID"), callerFromRequest(r, h.allowDevCallerHeader)); err != nil {
		handleError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) operations(w http.ResponseWriter, r *http.Request) {
	value, err := h.service.Operations(r.Context())
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (h *Handler) listManagedCollections(w http.ResponseWriter, r *http.Request) {
	limit, ok := collectionListLimit(w, r)
	if !ok {
		return
	}
	page, err := h.service.ListManagedCollections(r.Context(), authenticatedCaller(r), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) listAuthorizedCollections(w http.ResponseWriter, r *http.Request) {
	limit, ok := collectionListLimit(w, r)
	if !ok {
		return
	}
	page, err := h.service.ListAuthorizedCollections(r.Context(), collectionReaderSubject(r), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) collectionChanges(w http.ResponseWriter, r *http.Request) {
	collectionID := r.PathValue("collectionID")
	if !requireOpaqueID(w, collectionID, "collection ID") {
		return
	}
	page, err := h.service.CollectionChanges(r.Context(), collectionID, r.URL.Query().Get("cursor"), collectionReaderSubject(r))
	if err != nil {
		handleError(w, err)
		return
	}
	items := make([]collectionReaderItem, len(page.Items))
	for i, item := range page.Items {
		items[i] = readerItem(item)
	}
	writeJSON(w, http.StatusOK, collectionReaderChangePage{
		Collection: page.Collection,
		Items:      items,
		Tombstones: page.Tombstones,
		Cursor:     page.Cursor,
		HasMore:    page.HasMore,
		Reset:      page.Reset,
	})
}

func (h *Handler) getAuthorizedCollectionItem(w http.ResponseWriter, r *http.Request) {
	item, ok := h.authorizedCollectionItem(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, readerItem(item))
}

func (h *Handler) collectionContentPending(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authorizedCollectionItem(w, r); !ok {
		return
	}
	writeError(w, http.StatusNotImplemented, "AST_NOT_IMPLEMENTED", "collection content is not available")
}

func (h *Handler) authorizedCollectionItem(w http.ResponseWriter, r *http.Request) (assets.CollectionItem, bool) {
	collectionID, itemID := r.PathValue("collectionID"), r.PathValue("itemID")
	if !requireOpaqueID(w, collectionID, "collection ID") || !requireOpaqueID(w, itemID, "item ID") {
		return assets.CollectionItem{}, false
	}
	item, err := h.service.GetAuthorizedCollectionItem(r.Context(), collectionID, itemID, collectionReaderSubject(r))
	if err != nil {
		handleError(w, err)
		return assets.CollectionItem{}, false
	}
	return item, true
}

type collectionReaderItem struct {
	ID              string    `json:"id"`
	CollectionID    string    `json:"collectionId"`
	RemoteItemID    string    `json:"remoteItemId"`
	DisplayName     string    `json:"displayName"`
	SourceRevision  string    `json:"sourceRevision"`
	CreatedRevision int64     `json:"createdRevision"`
	DeletedRevision int64     `json:"deletedRevision,omitempty"`
	MIMEType        string    `json:"mimeType,omitempty"`
	SizeBytes       int64     `json:"sizeBytes,omitempty"`
	ETag            string    `json:"etag,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	DeletedAt       time.Time `json:"deletedAt,omitempty"`
}

type collectionReaderChangePage struct {
	Collection assets.Collection            `json:"collection"`
	Items      []collectionReaderItem       `json:"items"`
	Tombstones []assets.CollectionTombstone `json:"tombstones"`
	Cursor     string                       `json:"cursor"`
	HasMore    bool                         `json:"hasMore"`
	Reset      bool                         `json:"reset"`
}

func readerItem(item assets.CollectionItem) collectionReaderItem {
	return collectionReaderItem{
		ID:              item.ID,
		CollectionID:    item.CollectionID,
		RemoteItemID:    item.RemoteItemID,
		DisplayName:     item.DisplayName,
		SourceRevision:  item.SourceRevision,
		CreatedRevision: item.CreatedRevision,
		DeletedRevision: item.DeletedRevision,
		MIMEType:        item.MIMEType,
		SizeBytes:       item.SizeBytes,
		ETag:            item.ETag,
		CreatedAt:       item.CreatedAt,
		DeletedAt:       item.DeletedAt,
	}
}

func collectionListLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	value := r.URL.Query().Get("limit")
	if value == "" {
		return 0, true
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit <= 0 {
		writeError(w, http.StatusBadRequest, "AST_INVALID_REQUEST", "invalid limit")
		return 0, false
	}
	return limit, true
}

func (h *Handler) getManagedCollection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("collectionID")
	if !validOpaqueID(id) {
		writeError(w, http.StatusBadRequest, "AST_INVALID_REQUEST", "invalid collection ID")
		return
	}
	value, err := h.service.GetManagedCollection(r.Context(), id, authenticatedCaller(r))
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (h *Handler) createCollection(w http.ResponseWriter, r *http.Request) {
	caller, key, ok := collectionMutationIdentity(w, r)
	if !ok {
		return
	}
	var input assets.CreateCollectionInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if !validBytes(input.Name, 120) {
		writeError(w, http.StatusBadRequest, "AST_INVALID_REQUEST", "invalid collection name")
		return
	}
	input.CallerService, input.IdempotencyKey = caller, key
	value, err := h.service.CreateCollection(r.Context(), input)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, value)
}

func (h *Handler) renameCollection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("collectionID")
	caller, key, ok := collectionMutationIdentity(w, r)
	if !ok || !requireOpaqueID(w, id, "collection ID") {
		return
	}
	var input assets.RenameCollectionInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if !validBytes(input.Name, 120) {
		writeError(w, http.StatusBadRequest, "AST_INVALID_REQUEST", "invalid collection name")
		return
	}
	input.CollectionID, input.CallerService, input.IdempotencyKey = id, caller, key
	value, err := h.service.RenameCollection(r.Context(), input)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (h *Handler) deleteCollection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("collectionID")
	caller, key, ok := collectionMutationIdentity(w, r)
	if !ok || !requireOpaqueID(w, id, "collection ID") {
		return
	}
	value, err := h.service.DeleteCollection(r.Context(), assets.DeleteCollectionInput{CollectionID: id, CallerService: caller, IdempotencyKey: key})
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (h *Handler) addCollectionACL(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("collectionID")
	caller, key, ok := collectionMutationIdentity(w, r)
	if !ok || !requireOpaqueID(w, id, "collection ID") {
		return
	}
	var input assets.AddCollectionACLInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.CollectionID, input.CallerService, input.IdempotencyKey = id, caller, key
	value, err := h.service.AddCollectionACL(r.Context(), input)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, value)
}

func (h *Handler) revokeCollectionACL(w http.ResponseWriter, r *http.Request) {
	collectionID, aclID := r.PathValue("collectionID"), r.PathValue("aclID")
	caller, key, ok := collectionMutationIdentity(w, r)
	if !ok || !requireOpaqueID(w, collectionID, "collection ID") || !requireOpaqueID(w, aclID, "ACL ID") {
		return
	}
	value, err := h.service.RevokeCollectionACL(r.Context(), assets.RevokeCollectionACLInput{CollectionID: collectionID, ACLID: aclID, CallerService: caller, IdempotencyKey: key})
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (h *Handler) addCollectionItem(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("collectionID")
	caller, key, ok := collectionMutationIdentity(w, r)
	if !ok || !requireOpaqueID(w, id, "collection ID") {
		return
	}
	var input assets.AddCollectionItemInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	if !validOpaqueID(input.AssetID) || !validBytes(input.RemoteItemID, 255) || !validBytes(input.DisplayName, 255) {
		writeError(w, http.StatusBadRequest, "AST_INVALID_REQUEST", "invalid collection item")
		return
	}
	input.CollectionID, input.CallerService, input.IdempotencyKey = id, caller, key
	value, err := h.service.AddCollectionItem(r.Context(), input)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, value)
}

func (h *Handler) deleteCollectionItem(w http.ResponseWriter, r *http.Request) {
	collectionID, itemID := r.PathValue("collectionID"), r.PathValue("itemID")
	caller, key, ok := collectionMutationIdentity(w, r)
	if !ok || !requireOpaqueID(w, collectionID, "collection ID") || !requireOpaqueID(w, itemID, "item ID") {
		return
	}
	value, err := h.service.DeleteCollectionItem(r.Context(), assets.DeleteCollectionItemInput{CollectionID: collectionID, ItemID: itemID, CallerService: caller, IdempotencyKey: key})
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func collectionMutationIdentity(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	rawKey := r.Header.Get("Idempotency-Key")
	key := strings.TrimSpace(rawKey)
	if key == "" || len(rawKey) > 128 {
		writeError(w, http.StatusBadRequest, "AST_INVALID_REQUEST", "invalid Idempotency-Key")
		return "", "", false
	}
	return authenticatedCaller(r), key, true
}

func requireOpaqueID(w http.ResponseWriter, value, name string) bool {
	if validOpaqueID(value) {
		return true
	}
	writeError(w, http.StatusBadRequest, "AST_INVALID_REQUEST", "invalid "+name)
	return false
}

func validOpaqueID(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 255
}

func validBytes(value string, limit int) bool {
	return value != "" && len(value) <= limit
}

func (h *Handler) requireOwnedAsset(w http.ResponseWriter, r *http.Request) bool {
	asset, err := h.service.GetAsset(r.Context(), r.PathValue("assetID"))
	if err != nil {
		handleError(w, err)
		return false
	}
	if asset.OwnerService != callerFromRequest(r, h.allowDevCallerHeader) {
		writeError(w, http.StatusForbidden, "AST_FORBIDDEN", "caller does not own this asset")
		return false
	}
	return true
}

func (h *Handler) publicDownload(w http.ResponseWriter, r *http.Request) {
	h.servePublicDownload(w, r, "")
}

func (h *Handler) publicDerivativeDownload(w http.ResponseWriter, r *http.Request) {
	h.servePublicDownload(w, r, r.PathValue("variant"))
}

func (h *Handler) servePublicDownload(w http.ResponseWriter, r *http.Request, variant string) {
	metadata, err := h.service.PublicMetadata(r.Context(), r.PathValue("assetID"), variant)
	if err != nil {
		handleError(w, err)
		return
	}
	h.serveDownload(w, r, metadata)
}

func (h *Handler) serveDownload(w http.ResponseWriter, r *http.Request, metadata assets.PublicDownloadMetadata) {
	if matchesETag(r.Header.Get("If-None-Match"), metadata.ETag) {
		setPublicHeaders(w, metadata, -1, requestedDownloadName(r, metadata.FileName))
		w.WriteHeader(http.StatusNotModified)
		return
	}
	rangeValue := r.Header.Get("Range")
	if rangeValue != "" && !matchesIfRange(r.Header.Get("If-Range"), metadata) {
		rangeValue = ""
	}
	requested, partial, err := parseRange(rangeValue)
	if err != nil {
		writeUnsatisfiedRange(w, metadata.Size)
		return
	}
	contentLength := metadata.Size
	contentRange := ""
	if partial {
		requested, contentLength, err = resolveRange(requested, metadata.Size)
		if err != nil {
			writeUnsatisfiedRange(w, metadata.Size)
			return
		}
		end := requested.Offset + contentLength - 1
		contentRange = fmt.Sprintf("bytes %d-%d/%d", requested.Offset, end, metadata.Size)
	}
	if r.Method == http.MethodHead {
		setPublicHeaders(w, metadata, contentLength, requestedDownloadName(r, metadata.FileName))
		if partial {
			w.Header().Set("Content-Range", contentRange)
			w.WriteHeader(http.StatusPartialContent)
		}
		return
	}
	download, err := h.service.OpenPublicMetadata(r.Context(), metadata, requested)
	if err != nil {
		handleError(w, err)
		return
	}
	defer download.Body.Close()
	setPublicHeaders(w, metadata, contentLength, requestedDownloadName(r, metadata.FileName))
	if partial {
		w.Header().Set("Content-Range", contentRange)
		w.WriteHeader(http.StatusPartialContent)
	}
	if _, err := io.Copy(w, download.Body); err != nil {
		slog.Warn("stream asset download", "asset_id", r.PathValue("assetID"), "error", err)
	}
}

func setPublicHeaders(w http.ResponseWriter, metadata assets.PublicDownloadMetadata, contentLength int64, fileName string) {
	w.Header().Set("Content-Type", metadata.ContentType)
	if fileName != "" {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": fileName}))
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", metadata.CacheControl)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if contentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
	}
	if metadata.ETag != "" {
		w.Header().Set("ETag", metadata.ETag)
	}
	if !metadata.LastModified.IsZero() {
		w.Header().Set("Last-Modified", metadata.LastModified.UTC().Format(http.TimeFormat))
	}
}

func requestedDownloadName(r *http.Request, fallback string) string {
	requested := strings.TrimSpace(r.URL.Query().Get("filename"))
	if requested == "" || !strings.EqualFold(filepath.Ext(requested), filepath.Ext(fallback)) {
		return fallback
	}
	requested = strings.Map(func(value rune) rune {
		if value < 32 || strings.ContainsRune(`<>:"/\|?*`, value) {
			return '-'
		}
		return value
	}, requested)
	requested = strings.Trim(strings.TrimSpace(requested), ".")
	runes := []rune(requested)
	if len(runes) > 180 {
		requested = string(runes[:180-len([]rune(filepath.Ext(requested)))]) + filepath.Ext(requested)
	}
	if requested == "" || requested == filepath.Ext(requested) {
		return fallback
	}
	return requested
}

func writeUnsatisfiedRange(w http.ResponseWriter, total int64) {
	w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
	writeError(w, http.StatusRequestedRangeNotSatisfiable, "AST_INVALID_RANGE", "invalid range")
}

func resolveRange(requested assets.ByteRange, total int64) (assets.ByteRange, int64, error) {
	if requested.Suffix > 0 {
		if total <= 0 {
			return assets.ByteRange{}, 0, fmt.Errorf("suffix range for empty content")
		}
		count := min(requested.Suffix, total)
		return assets.ByteRange{Offset: total - count, Count: count}, count, nil
	}
	if requested.Offset >= total {
		return assets.ByteRange{}, 0, fmt.Errorf("range starts beyond content")
	}
	count := requested.Count
	remaining := total - requested.Offset
	if count == 0 || count > remaining {
		count = remaining
	}
	requested.Count = count
	return requested, count, nil
}

func matchesETag(value, current string) bool {
	if value == "" || current == "" {
		return false
	}
	current = strings.TrimPrefix(strings.TrimSpace(current), "W/")
	for _, candidate := range strings.Split(value, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == current {
			return true
		}
	}
	return false
}

func matchesIfRange(value string, metadata assets.PublicDownloadMetadata) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	if strings.HasPrefix(value, "W/") {
		return false
	}
	current := strings.TrimSpace(metadata.ETag)
	if strings.HasPrefix(value, `"`) {
		return !strings.HasPrefix(current, "W/") && value == current
	}
	date, err := http.ParseTime(value)
	if err != nil || metadata.LastModified.IsZero() {
		return false
	}
	return !metadata.LastModified.UTC().Truncate(time.Second).After(date.UTC())
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(w, http.StatusBadRequest, "AST_INVALID_REQUEST", "invalid request body")
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "AST_INVALID_REQUEST", "invalid request body")
		return false
	}
	return true
}
func handleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, assets.ErrInvalidInput), errors.Is(err, assets.ErrInvalidUpload):
		writeError(w, http.StatusBadRequest, "AST_INVALID_REQUEST", err.Error())
	case errors.Is(err, assets.ErrForbidden):
		writeError(w, http.StatusForbidden, "AST_FORBIDDEN", "operation is not allowed")
	case errors.Is(err, assets.ErrConflict):
		writeError(w, http.StatusConflict, "AST_CONFLICT", "idempotency key conflicts with an existing request")
	case errors.Is(err, assets.ErrNotFound):
		writeError(w, http.StatusNotFound, "AST_NOT_FOUND", "asset not found")
	default:
		writeError(w, http.StatusInternalServerError, "AST_INTERNAL", "internal error")
	}
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func parseRange(value string) (assets.ByteRange, bool, error) {
	if value == "" {
		return assets.ByteRange{}, false, nil
	}
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return assets.ByteRange{}, false, fmt.Errorf("invalid range")
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 {
		return assets.ByteRange{}, false, fmt.Errorf("invalid range")
	}
	if parts[0] == "" {
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix <= 0 {
			return assets.ByteRange{}, false, fmt.Errorf("invalid range")
		}
		return assets.ByteRange{Suffix: suffix}, true, nil
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 {
		return assets.ByteRange{}, false, fmt.Errorf("invalid range")
	}
	count := int64(0)
	if parts[1] != "" {
		end, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || end < start {
			return assets.ByteRange{}, false, fmt.Errorf("invalid range")
		}
		count = end - start + 1
		if count <= 0 {
			return assets.ByteRange{}, false, fmt.Errorf("invalid range")
		}
	}
	return assets.ByteRange{Offset: start, Count: count}, true, nil
}
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-HHC-Request-ID")
		if id == "" {
			id = strconv.FormatInt(time.Now().UnixNano(), 36)
		}
		w.Header().Set("X-HHC-Request-ID", id)
		next.ServeHTTP(w, r)
	})
}

func callerFromRequest(r *http.Request, allowDevelopmentHeader bool) string {
	if caller := authenticatedCaller(r); caller != "" {
		return caller
	}
	if caller := strings.TrimSpace(r.Header.Get("Dapr-Caller-App-Id")); caller != "" {
		return caller
	}
	if allowDevelopmentHeader {
		return strings.TrimSpace(r.Header.Get("X-Internal-Caller-App-Id"))
	}
	return ""
}

func authenticatedCaller(r *http.Request) string {
	caller, _ := r.Context().Value(callerContextKey{}).(string)
	return caller
}

type collectionReaderIdentity struct {
	Subject        assets.CollectionSubject
	TokenID        string
	TokenExpiresAt time.Time
	SessionID      string
	AuthProvider   string
}

type collectionReaderIdentityKey struct{}

func parseCollectionReaderIdentity(r *http.Request) (collectionReaderIdentity, bool) {
	userID := strings.TrimSpace(r.Header.Get("X-HHC-User-ID"))
	expires, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get("X-HHC-Token-Expires-At")), 10, 64)
	if userID == "" || err != nil || expires <= 0 {
		return collectionReaderIdentity{}, false
	}
	roles := []string{}
	seen := map[string]bool{}
	for _, value := range strings.Split(r.Header.Get("X-HHC-Roles"), ",") {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			roles = append(roles, value)
		}
	}
	return collectionReaderIdentity{
		Subject: assets.CollectionSubject{UserID: userID, Roles: roles}, TokenID: strings.TrimSpace(r.Header.Get("X-HHC-Token-ID")),
		TokenExpiresAt: time.Unix(expires, 0).UTC(), SessionID: strings.TrimSpace(r.Header.Get("X-HHC-Session-ID")),
		AuthProvider: strings.TrimSpace(r.Header.Get("X-HHC-Auth-Provider")),
	}, true
}

func collectionReaderSubject(r *http.Request) assets.CollectionSubject {
	identity, _ := r.Context().Value(collectionReaderIdentityKey{}).(collectionReaderIdentity)
	return identity.Subject
}

type callerContextKey struct{}

func workloadCaller(encoded string, config WorkloadAuthConfig) string {
	if encoded == "" || config.TenantID == "" || config.Issuer == "" || config.Audience == "" || config.RequiredRole == "" {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return ""
	}
	var principal struct {
		AuthType string `json:"auth_typ"`
		Claims   []struct {
			Type  string `json:"typ"`
			Value string `json:"val"`
		} `json:"claims"`
	}
	if json.Unmarshal(raw, &principal) != nil || principal.AuthType != "aad" {
		return ""
	}
	claims := map[string][]string{}
	for _, claim := range principal.Claims {
		claims[claim.Type] = append(claims[claim.Type], claim.Value)
	}
	claim := func(names ...string) string {
		for _, name := range names {
			if len(claims[name]) > 0 {
				return claims[name][0]
			}
		}
		return ""
	}
	if claim("tid", "http://schemas.microsoft.com/identity/claims/tenantid") != config.TenantID || !validWorkloadIssuer(claim("iss"), config) || claim("aud") != config.Audience {
		return ""
	}
	clientID := claim("appid", "azp")
	configured, ok := config.Callers[clientID]
	if !ok || claim("oid", "http://schemas.microsoft.com/identity/claims/objectidentifier") != configured.ObjectID {
		return ""
	}
	for _, role := range append(claims["roles"], claims["http://schemas.microsoft.com/ws/2008/06/identity/claims/role"]...) {
		if role == config.RequiredRole {
			return configured.Service
		}
	}
	return ""
}

func validWorkloadIssuer(issuer string, config WorkloadAuthConfig) bool {
	return issuer == config.Issuer || issuer == "https://sts.windows.net/"+config.TenantID+"/"
}
