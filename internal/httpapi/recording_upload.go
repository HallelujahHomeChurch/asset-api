package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"hhc/asset-api/internal/assets"
)

func (h *Handler) recordingUploadAllowed(w http.ResponseWriter, r *http.Request) bool {
	if callerFromRequest(r, h.allowDevCallerHeader) != "hhc-web-api" {
		writeError(w, http.StatusForbidden, "AST_FORBIDDEN", "caller cannot manage recordings")
		return false
	}
	if h.recordingUpload == nil {
		writeError(w, http.StatusServiceUnavailable, "AST_UNAVAILABLE", "recording storage is unavailable")
		return false
	}
	return true
}

func (h *Handler) getRecordingAsset(w http.ResponseWriter, r *http.Request) {
	if callerFromRequest(r, h.allowDevCallerHeader) != "hhc-web-api" {
		writeError(w, http.StatusForbidden, "AST_FORBIDDEN", "caller cannot read recordings")
		return
	}
	if h.recordingAssets == nil {
		writeError(w, http.StatusServiceUnavailable, "AST_UNAVAILABLE", "recording storage is unavailable")
		return
	}
	asset, err := h.recordingAssets.GetByVersion(r.Context(), r.PathValue("assetVersionID"))
	if err != nil {
		handleError(w, err)
		return
	}
	if asset.Status == "completing" {
		if h.recordingUpload == nil {
			writeError(w, http.StatusServiceUnavailable, "AST_UNAVAILABLE", "recording recovery is unavailable")
			return
		}
		asset, err = h.recordingUpload.Get(r.Context(), asset.ID, asset.AdminID)
		if err != nil {
			handleError(w, err)
			return
		}
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, map[string]any{"assetVersionId": asset.AssetVersionID, "recordingId": asset.RecordingID, "status": asset.Status, "sizeBytes": asset.SizeBytes, "durationSeconds": asset.DurationSeconds, "uploadedAt": asset.UploadedAt})
}

func (h *Handler) issueRecordingGrant(w http.ResponseWriter, r *http.Request) {
	if callerFromRequest(r, h.allowDevCallerHeader) != "hhc-web-api" {
		writeError(w, http.StatusForbidden, "AST_FORBIDDEN", "caller cannot grant recordings")
		return
	}
	if h.recordingAssets == nil || h.recordingSigner == nil {
		writeError(w, http.StatusServiceUnavailable, "AST_UNAVAILABLE", "recording grants are unavailable")
		return
	}
	var input struct {
		RecordingID        string    `json:"recordingId"`
		ScopeID            string    `json:"scopeId"`
		RecordingExpiresAt time.Time `json:"recordingExpiresAt"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	asset, err := h.recordingAssets.GetByVersion(r.Context(), r.PathValue("assetVersionID"))
	if err != nil {
		handleError(w, err)
		return
	}
	if asset.Status != "ready" || asset.OwnerService != "hhc-web-api" || asset.RecordingID != input.RecordingID {
		handleError(w, assets.ErrForbidden)
		return
	}
	grant, err := h.recordingSigner.Issue(input.RecordingID, asset.AssetVersionID, input.ScopeID, asset.ObjectKey, input.RecordingExpiresAt, time.Now())
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	writeJSON(w, http.StatusOK, grant)
}

func (h *Handler) deleteRecordingAsset(w http.ResponseWriter, r *http.Request) {
	if !h.recordingUploadAllowed(w, r) {
		return
	}
	if err := h.recordingUpload.DeleteAsset(r.Context(), r.PathValue("assetVersionID")); err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) createRecordingUpload(w http.ResponseWriter, r *http.Request) {
	if !h.recordingUploadAllowed(w, r) {
		return
	}
	var input assets.CreateRecordingUploadInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.AdminID = strings.TrimSpace(r.Header.Get("X-HHC-Actor-ID"))
	input.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	session, err := h.recordingUpload.Create(r.Context(), input)
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusCreated, session)
}

func (h *Handler) getRecordingUpload(w http.ResponseWriter, r *http.Request) {
	if !h.recordingUploadAllowed(w, r) {
		return
	}
	session, err := h.recordingUpload.Get(r.Context(), r.PathValue("sessionID"), strings.TrimSpace(r.Header.Get("X-HHC-Actor-ID")))
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, session)
}

func (h *Handler) listRecordingParts(w http.ResponseWriter, r *http.Request) {
	if !h.recordingUploadAllowed(w, r) {
		return
	}
	numbers, err := h.recordingUpload.UploadedParts(r.Context(), r.PathValue("sessionID"), strings.TrimSpace(r.Header.Get("X-HHC-Actor-ID")))
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, numbers)
}

func (h *Handler) signRecordingPart(w http.ResponseWriter, r *http.Request) {
	if !h.recordingUploadAllowed(w, r) {
		return
	}
	number, err := strconv.Atoi(r.PathValue("partNumber"))
	if err != nil {
		handleError(w, assets.ErrInvalidInput)
		return
	}
	part, err := h.recordingUpload.SignPart(r.Context(), r.PathValue("sessionID"), strings.TrimSpace(r.Header.Get("X-HHC-Actor-ID")), number)
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, part)
}

func (h *Handler) completeRecordingUpload(w http.ResponseWriter, r *http.Request) {
	if !h.recordingUploadAllowed(w, r) {
		return
	}
	session, err := h.recordingUpload.Complete(r.Context(), r.PathValue("sessionID"), strings.TrimSpace(r.Header.Get("X-HHC-Actor-ID")))
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, session)
}

func (h *Handler) abortRecordingUpload(w http.ResponseWriter, r *http.Request) {
	if !h.recordingUploadAllowed(w, r) {
		return
	}
	if err := h.recordingUpload.Abort(r.Context(), r.PathValue("sessionID"), strings.TrimSpace(r.Header.Get("X-HHC-Actor-ID"))); err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusNoContent)
}
