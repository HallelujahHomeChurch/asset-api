package httpapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/storage/r2"
	"io"
	"net/http"
	"strconv"
	"strings"
)

type RecordingCoverStore interface {
	Create(context.Context, string, string, string, string, string, string) (assets.RecordingCover, error)
	Queue(context.Context, string) error
	List(context.Context, string, string) ([]assets.RecordingCover, error)
	Get(context.Context, string, string, string) (assets.RecordingCover, error)
	Retain(context.Context, string, string, string, string) error
	Release(context.Context, string, string, string, string) error
}

func (h *Handler) WithRecordingCovers(store RecordingCoverStore, objects *r2.Store) *Handler {
	h.recordingCovers = store
	h.coverObjects = objects
	return h
}
func (h *Handler) coverAllowed(w http.ResponseWriter, r *http.Request) bool {
	if callerFromRequest(r, h.allowDevCallerHeader) != "hhc-web-api" {
		writeError(w, 403, "AST_FORBIDDEN", "caller cannot manage recording covers")
		return false
	}
	if h.recordingCovers == nil || h.coverObjects == nil {
		writeError(w, 503, "AST_UNAVAILABLE", "recording covers unavailable")
		return false
	}
	w.Header().Set("Cache-Control", "private, no-store")
	return true
}

type coverItem struct {
	ID           string `json:"id"`
	UploadID     string `json:"uploadId"`
	Kind         string `json:"kind"`
	State        string `json:"state"`
	OperationKey string `json:"operationKey,omitempty"`
}

func (h *Handler) listRecordingCovers(w http.ResponseWriter, r *http.Request) {
	if !h.coverAllowed(w, r) {
		return
	}
	values, err := h.recordingCovers.List(r.Context(), r.PathValue("packageID"), r.URL.Query().Get("recordingId"))
	if err != nil {
		handleError(w, err)
		return
	}
	items := []coverItem{}
	for _, c := range values {
		start, end := 0, 0
		if c.Kind == "auto" {
			start, end = 1, 3
		}
		for i := start; i <= end; i++ {
			items = append(items, coverItem{ID: fmt.Sprintf("%s-%d", c.ID, i), UploadID: c.ID, Kind: c.Kind, State: c.State, OperationKey: c.OperationKey})
		}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
func (h *Handler) uploadRecordingCover(w http.ResponseWriter, r *http.Request) {
	if !h.coverAllowed(w, r) {
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, assets.RecordingCoverInputMaxBytes))
	if err != nil {
		writeError(w, 413, "cover_too_large", "cover exceeds 5 MiB")
		return
	}
	mime := r.Header.Get("Content-Type")
	if _, err := assets.ValidateRecordingCoverInput(data, mime); err != nil {
		writeError(w, 422, "invalid_cover", "invalid JPEG/PNG or unsupported image dimensions/orientation")
		return
	}
	c, err := h.recordingCovers.Create(r.Context(), r.PathValue("packageID"), r.URL.Query().Get("recordingId"), r.Header.Get("X-HHC-Actor-ID"), r.Header.Get("Idempotency-Key"), mime, fmt.Sprintf("%x", sha256.Sum256(data)))
	if err != nil {
		handleError(w, err)
		return
	}
	if c.State == "uploading" {
		if err := h.coverObjects.PutCoverInput(r.Context(), c.InputKey(), data, mime); err != nil {
			writeError(w, 503, "AST_UNAVAILABLE", "cover upload unavailable")
			return
		}
		if err := h.recordingCovers.Queue(r.Context(), c.ID); err != nil {
			handleError(w, err)
			return
		}
		c.State = "pending"
	}
	writeJSON(w, 202, map[string]string{"uploadId": c.ID, "state": c.State})
}
func (h *Handler) resolveCover(r *http.Request) (assets.RecordingCover, int, error) {
	raw := r.PathValue("coverID")
	pos := strings.LastIndexByte(raw, '-')
	if pos < 1 {
		return assets.RecordingCover{}, 0, assets.ErrNotFound
	}
	index, err := strconv.Atoi(raw[pos+1:])
	if err != nil {
		return assets.RecordingCover{}, 0, assets.ErrNotFound
	}
	c, err := h.recordingCovers.Get(r.Context(), r.PathValue("packageID"), r.URL.Query().Get("recordingId"), raw[:pos])
	if err != nil {
		return c, 0, err
	}
	if c.State != "ready" || c.Kind == "auto" && (index < 1 || index > 3) || c.Kind == "custom" && index != 0 {
		return c, 0, assets.ErrNotFound
	}
	return c, index, nil
}
func (h *Handler) readRecordingCover(w http.ResponseWriter, r *http.Request) {
	if !h.coverAllowed(w, r) {
		return
	}
	c, index, err := h.resolveCover(r)
	if err != nil {
		handleError(w, err)
		return
	}
	body, err := h.coverObjects.Open(r.Context(), c.Key(index))
	if err != nil {
		writeError(w, 503, "AST_UNAVAILABLE", "cover unavailable")
		return
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, assets.RecordingCoverOutputMaxBytes+1))
	if err != nil || len(data) > assets.RecordingCoverOutputMaxBytes {
		writeError(w, 503, "AST_UNAVAILABLE", "cover unavailable")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(data)
}
func (h *Handler) retainRecordingCover(w http.ResponseWriter, r *http.Request) {
	if !h.coverAllowed(w, r) {
		return
	}
	c, _, err := h.resolveCover(r)
	if err != nil {
		handleError(w, err)
		return
	}
	var input struct {
		ReferenceID string `json:"referenceId"`
	}
	if !decodeJSONLimit(w, r, &input, 2048) {
		return
	}
	if err := h.recordingCovers.Retain(r.Context(), c.PackageID, c.RecordingID, c.ID, input.ReferenceID); err != nil {
		handleError(w, err)
		return
	}
	w.WriteHeader(204)
}
func (h *Handler) releaseRecordingCover(w http.ResponseWriter, r *http.Request) {
	if !h.coverAllowed(w, r) {
		return
	}
	raw := r.PathValue("coverID")
	pos := strings.LastIndexByte(raw, '-')
	if pos < 1 {
		handleError(w, assets.ErrInvalidInput)
		return
	}
	if err := h.recordingCovers.Release(r.Context(), r.PathValue("packageID"), r.URL.Query().Get("recordingId"), raw[:pos], r.PathValue("referenceID")); err != nil {
		handleError(w, err)
		return
	}
	w.WriteHeader(204)
}
