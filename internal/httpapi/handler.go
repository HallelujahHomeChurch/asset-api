package httpapi

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	localUpload          http.HandlerFunc
}

func New(service *assets.Service, db *sql.DB, allowedCallers map[string]bool, allowDevCallerHeader bool, appAPIToken string, localUpload http.HandlerFunc) *Handler {
	return &Handler{service: service, db: db, allowedCallers: allowedCallers, allowDevCallerHeader: allowDevCallerHeader, appAPIToken: appAPIToken, localUpload: localUpload}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "healthy"})
	})
	mux.HandleFunc("GET /ready", h.ready)
	mux.HandleFunc("GET /api/assets/public/{assetID}", h.publicDownload)
	mux.HandleFunc("GET /api/assets/public/{assetID}/{variant}", h.publicDerivativeDownload)
	mux.Handle("POST /priv/assets/upload-sessions", h.internal(http.HandlerFunc(h.createUpload)))
	mux.Handle("GET /priv/assets/operations", h.internal(http.HandlerFunc(h.operations)))
	mux.Handle("GET /priv/assets/{assetID}", h.internal(http.HandlerFunc(h.getAsset)))
	mux.Handle("POST /priv/assets/{assetID}/complete", h.internal(http.HandlerFunc(h.completeUpload)))
	mux.Handle("POST /priv/assets/{assetID}/grants", h.internal(http.HandlerFunc(h.createGrant)))
	mux.Handle("DELETE /priv/assets/{assetID}/grants/{grantID}", h.internal(http.HandlerFunc(h.revokeGrant)))
	mux.Handle("POST /priv/assets/{assetID}/scan/requeue", h.internal(http.HandlerFunc(h.requeueScan)))
	mux.Handle("DELETE /priv/assets/{assetID}", h.internal(http.HandlerFunc(h.deleteAsset)))
	mux.Handle("GET /priv/assets/{assetID}/public-url", h.internal(http.HandlerFunc(h.publicURL)))
	if h.localUpload != nil {
		mux.Handle("/dev/uploads/{token}", localUploadCORS(http.HandlerFunc(h.localUpload)))
	}
	return requestID(mux)
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
		if !h.allowDevCallerHeader && !sameToken(r.Header.Get("dapr-api-token"), h.appAPIToken) {
			writeError(w, http.StatusForbidden, "AST_FORBIDDEN", "invalid app token")
			return
		}
		caller := callerFromRequest(r, h.allowDevCallerHeader)
		if !h.allowedCallers[caller] {
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
	if matchesETag(r.Header.Get("If-None-Match"), metadata.ETag) {
		setPublicHeaders(w, metadata, -1)
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
		setPublicHeaders(w, metadata, contentLength)
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
	setPublicHeaders(w, metadata, contentLength)
	if partial {
		w.Header().Set("Content-Range", contentRange)
		w.WriteHeader(http.StatusPartialContent)
	}
	_, _ = io.Copy(w, download.Body)
}

func setPublicHeaders(w http.ResponseWriter, metadata assets.PublicDownloadMetadata, contentLength int64) {
	w.Header().Set("Content-Type", metadata.ContentType)
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
	if caller := strings.TrimSpace(r.Header.Get("Dapr-Caller-App-Id")); caller != "" {
		return caller
	}
	if allowDevelopmentHeader {
		return strings.TrimSpace(r.Header.Get("X-Internal-Caller-App-Id"))
	}
	return ""
}
