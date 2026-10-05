package httpapi

import (
	"context"
	"net/http"
	"strings"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/auditclient"
)

type RecordingRetentionStore interface {
	RecordingLifecycle(context.Context, []assets.RecordingLifecycleBinding) (assets.RecordingLifecycleSnapshot, error)
	GetRetentionPolicy(context.Context) (assets.RecordingRetentionPolicy, error)
	PreviewRetentionPolicy(context.Context, int) (assets.RecordingRetentionPreview, error)
	UpdateRetentionPolicy(context.Context, assets.UpdateRecordingRetentionInput) (assets.RecordingRetentionPolicy, error)
}

func (h *Handler) recordingLifecycle(w http.ResponseWriter, r *http.Request) {
	if !h.recordingRetentionAllowed(w, r, false) {
		return
	}
	var in struct {
		Items []assets.RecordingLifecycleBinding `json:"items"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	v, err := h.recordingRetention.RecordingLifecycle(r.Context(), in.Items)
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, v)
}

func (h *Handler) WithRecordingRetention(store RecordingRetentionStore) *Handler {
	h.recordingRetention = store
	return h
}

func (h *Handler) recordingRetentionAllowed(w http.ResponseWriter, r *http.Request, mutation bool) bool {
	if !h.recordingPackageAllowed(w, r) {
		return false
	}
	if h.recordingRetention == nil {
		writeError(w, http.StatusServiceUnavailable, "AST_UNAVAILABLE", "recording retention is unavailable")
		return false
	}
	if mutation {
		p, ok := auditclient.ProvenanceFromContext(r.Context())
		if !ok || p.ActorType != "user" {
			handleError(w, assets.ErrForbidden)
			return false
		}
	}
	return true
}

func (h *Handler) getRecordingRetention(w http.ResponseWriter, r *http.Request) {
	if !h.recordingRetentionAllowed(w, r, false) {
		return
	}
	p, err := h.recordingRetention.GetRetentionPolicy(r.Context())
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) previewRecordingRetention(w http.ResponseWriter, r *http.Request) {
	if !h.recordingRetentionAllowed(w, r, true) {
		return
	}
	var in struct {
		RetentionDays int `json:"retentionDays"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.RetentionDays < 1 || in.RetentionDays > 365 {
		handleError(w, assets.ErrInvalidInput)
		return
	}
	p, err := h.recordingRetention.PreviewRetentionPolicy(r.Context(), in.RetentionDays)
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) updateRecordingRetention(w http.ResponseWriter, r *http.Request) {
	if !h.recordingRetentionAllowed(w, r, true) {
		return
	}
	var in assets.UpdateRecordingRetentionInput
	if !decodeJSON(w, r, &in) {
		return
	}
	in.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	p, err := h.recordingRetention.UpdateRetentionPolicy(r.Context(), in)
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, p)
}
