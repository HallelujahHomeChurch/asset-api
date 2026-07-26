package httpapi

import (
	"context"
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
	localUpload          http.HandlerFunc
}

func New(service *assets.Service, db *sql.DB, allowedCallers map[string]bool, allowDevCallerHeader bool, localUpload http.HandlerFunc) *Handler {
	return &Handler{service: service, db: db, allowedCallers: allowedCallers, allowDevCallerHeader: allowDevCallerHeader, localUpload: localUpload}
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
	mux.Handle("GET /priv/assets/{assetID}", h.internal(http.HandlerFunc(h.getAsset)))
	mux.Handle("POST /priv/assets/{assetID}/complete", h.internal(http.HandlerFunc(h.completeUpload)))
	mux.Handle("POST /priv/assets/{assetID}/grants", h.internal(http.HandlerFunc(h.createGrant)))
	mux.Handle("DELETE /priv/assets/{assetID}/grants/{grantID}", h.internal(http.HandlerFunc(h.revokeGrant)))
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
		caller := callerFromRequest(r, h.allowDevCallerHeader)
		if !h.allowedCallers[caller] {
			writeError(w, http.StatusForbidden, "AST_FORBIDDEN", "caller is not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
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
	requested, partial, err := parseRange(r.Header.Get("Range"))
	if err != nil {
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "AST_INVALID_RANGE", "invalid range")
		return
	}
	var download assets.BlobDownload
	if variant == "" {
		download, err = h.service.OpenPublic(r.Context(), r.PathValue("assetID"), requested)
	} else {
		download, err = h.service.OpenPublicVariant(r.Context(), r.PathValue("assetID"), variant, requested)
	}
	if err != nil {
		handleError(w, err)
		return
	}
	defer download.Body.Close()
	w.Header().Set("Content-Type", download.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(download.Size, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if download.ETag != "" {
		w.Header().Set("ETag", download.ETag)
	}
	if !download.LastModified.IsZero() {
		w.Header().Set("Last-Modified", download.LastModified.UTC().Format(http.TimeFormat))
	}
	if partial {
		end := requested.Offset + download.Size - 1
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", requested.Offset, end, download.TotalSize))
		w.WriteHeader(http.StatusPartialContent)
	}
	_, _ = io.Copy(w, download.Body)
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
	if len(parts) != 2 || parts[0] == "" {
		return assets.ByteRange{}, false, fmt.Errorf("invalid range")
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
