package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"hhc/asset-api/internal/assets"
)

func (h *Handler) recordingSourceAllowed(w http.ResponseWriter, r *http.Request) bool {
	if callerFromRequest(r, h.allowDevCallerHeader) != "hhc-web-api" {
		writeError(w, http.StatusForbidden, "AST_FORBIDDEN", "caller cannot manage recording sources")
		return false
	}
	if h.recordingSources == nil {
		writeError(w, http.StatusServiceUnavailable, "AST_UNAVAILABLE", "recording sources are not enabled")
		return false
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	return true
}

func (h *Handler) createRecordingSource(w http.ResponseWriter, r *http.Request) {
	if !h.recordingSourceAllowed(w, r) {
		return
	}
	var input assets.CreateRecordingSourceInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.ActorID = strings.TrimSpace(r.Header.Get("X-HHC-Actor-ID"))
	input.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	p, err := h.recordingSources.Create(r.Context(), input)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) getRecordingSource(w http.ResponseWriter, r *http.Request) {
	if !h.recordingSourceAllowed(w, r) {
		return
	}
	cursor, limit := 0, 100
	for key, target := range map[string]*int{"cursor": &cursor, "limit": &limit} {
		if raw := r.URL.Query().Get(key); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil {
				handleError(w, assets.ErrInvalidInput)
				return
			}
			*target = value
		}
	}
	page, err := h.recordingSources.Status(r.Context(), r.PathValue("sourceID"), strings.TrimSpace(r.Header.Get("X-HHC-Actor-ID")), cursor, limit)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) signRecordingSource(w http.ResponseWriter, r *http.Request) {
	if !h.recordingSourceAllowed(w, r) {
		return
	}
	var input struct {
		Numbers []int `json:"numbers"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	signed, err := h.recordingSources.Sign(r.Context(), r.PathValue("sourceID"), strings.TrimSpace(r.Header.Get("X-HHC-Actor-ID")), input.Numbers)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, signed)
}

func (h *Handler) completeRecordingSource(w http.ResponseWriter, r *http.Request) {
	if !h.recordingSourceAllowed(w, r) {
		return
	}
	p, err := h.recordingSources.Complete(r.Context(), r.PathValue("sourceID"), strings.TrimSpace(r.Header.Get("X-HHC-Actor-ID")))
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, p)
}
