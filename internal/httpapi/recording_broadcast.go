package httpapi

import (
	"errors"
	"hhc/asset-api/internal/assets"
	"net/http"
	"time"
)

func (h *Handler) WithRecordingBroadcasts(repo assets.RecordingBroadcastRepository) *Handler {
	h.recordingBroadcasts = repo
	return h
}
func (h *Handler) broadcastAllowed(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "private, no-store")
	if callerFromRequest(r, h.allowDevCallerHeader) != "hhc-web-api" {
		writeError(w, 403, "AST_FORBIDDEN", "caller cannot manage broadcast ranges")
		return false
	}
	if h.recordingCaptures == nil || h.recordingBroadcasts == nil {
		writeError(w, 503, "AST_NOT_READY", "broadcast ranges unavailable")
		return false
	}
	return true
}
func broadcastError(w http.ResponseWriter, err error) {
	status, code := 503, "AST_NOT_READY"
	switch {
	case errors.Is(err, assets.ErrInvalidInput):
		status, code = 400, "AST_INVALID_REQUEST"
	case errors.Is(err, assets.ErrForbidden):
		status, code = 403, "AST_FORBIDDEN"
	case errors.Is(err, assets.ErrNotFound):
		status, code = 404, "AST_NOT_FOUND"
	case errors.Is(err, assets.ErrConflict):
		status, code = 409, "AST_CONFLICT"
	}
	writeError(w, status, code, code)
}
func (h *Handler) setRecordingBroadcastRange(w http.ResponseWriter, r *http.Request) {
	if !h.broadcastAllowed(w, r) {
		return
	}
	var input assets.RecordingBroadcastRange
	if !decodeJSON(w, r, &input) {
		return
	}
	result, err := h.recordingBroadcasts.SetBroadcastRange(r.Context(), r.PathValue("captureID"), input)
	if err != nil {
		broadcastError(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func (h *Handler) getRecordingBroadcastProjection(w http.ResponseWriter, r *http.Request) {
	if !h.broadcastAllowed(w, r) {
		return
	}
	result, err := h.recordingBroadcasts.GetBroadcastProjection(r.Context(), r.PathValue("captureID"))
	if err != nil {
		broadcastError(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func (h *Handler) grantRecordingPreview(w http.ResponseWriter, r *http.Request) {
	if !h.broadcastAllowed(w, r) {
		return
	}
	var input struct {
		RecordingID     string    `json:"recordingId"`
		Epoch           int64     `json:"epoch"`
		UserID          string    `json:"userId"`
		PlaybackScopeID string    `json:"playbackScopeId"`
		ExpiresAt       time.Time `json:"expiresAt"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	grant, err := h.recordingCaptures.GrantPreview(r.Context(), h.recordingSigner, r.PathValue("captureID"), input.RecordingID, input.UserID, input.PlaybackScopeID, input.Epoch, input.ExpiresAt)
	if err != nil {
		broadcastError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"recordingId": input.RecordingID, "captureId": r.PathValue("captureID"), "epoch": input.Epoch, "purpose": "staff-preview", "scope": input.PlaybackScopeID, "mediaPath": "/videos/" + input.RecordingID + "/captures/" + r.PathValue("captureID") + "/previews/" + input.PlaybackScopeID + "/master.m3u8", "exchangeCredential": grant.ExchangeCredential, "issuedAt": time.Now().UTC(), "expiresAt": grant.ExpiresAt})
}
