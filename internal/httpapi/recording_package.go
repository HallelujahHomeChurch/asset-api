package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"hhc/asset-api/internal/assets"
)

func (h *Handler) recordingPackageAllowed(w http.ResponseWriter, r *http.Request) bool {
	if callerFromRequest(r, h.allowDevCallerHeader) != "hhc-web-api" {
		writeError(w, http.StatusForbidden, "AST_FORBIDDEN", "caller cannot manage recording packages")
		return false
	}
	if h.recordingPackages == nil {
		writeError(w, http.StatusServiceUnavailable, "AST_UNAVAILABLE", "recording packages are not enabled")
		return false
	}
	return true
}

func (h *Handler) issueRecordingPackageGrant(w http.ResponseWriter, r *http.Request) {
	if !h.recordingPackageAllowed(w, r) {
		return
	}
	if h.recordingSigner == nil {
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
	p, err := h.recordingPackages.GetReady(r.Context(), r.PathValue("packageID"), input.RecordingID)
	if err != nil {
		handleError(w, err)
		return
	}
	grant, err := h.recordingSigner.IssuePackage(p, input.ScopeID, input.RecordingExpiresAt, time.Now())
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	writeJSON(w, http.StatusOK, grant)
}

func (h *Handler) createRecordingPackage(w http.ResponseWriter, r *http.Request) {
	if !h.recordingPackageAllowed(w, r) {
		return
	}
	var input struct {
		RecordingID string          `json:"recordingId"`
		Inventory   json.RawMessage `json:"inventory"`
	}
	if !decodeJSONLimit(w, r, &input, assets.RecordingInventoryMaxBytes+1024) {
		return
	}
	inv, err := assets.DecodeRecordingInventory(bytes.NewReader(input.Inventory))
	if err != nil {
		handleError(w, err)
		return
	}
	p, err := h.recordingPackages.Create(r.Context(), assets.CreateRecordingPackageInput{
		ActorID: strings.TrimSpace(r.Header.Get("X-HHC-Actor-ID")), IdempotencyKey: strings.TrimSpace(r.Header.Get("Idempotency-Key")), RecordingID: input.RecordingID, Inventory: inv,
	})
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) getRecordingPackage(w http.ResponseWriter, r *http.Request) {
	if !h.recordingPackageAllowed(w, r) {
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil {
			handleError(w, assets.ErrInvalidInput)
			return
		}
	}
	page, err := h.recordingPackages.Status(r.Context(), r.PathValue("packageID"), strings.TrimSpace(r.Header.Get("X-HHC-Actor-ID")), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) signRecordingPackage(w http.ResponseWriter, r *http.Request) {
	if !h.recordingPackageAllowed(w, r) {
		return
	}
	var input struct {
		Paths []string `json:"paths"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	signed, err := h.recordingPackages.Sign(r.Context(), r.PathValue("packageID"), strings.TrimSpace(r.Header.Get("X-HHC-Actor-ID")), input.Paths)
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	writeJSON(w, http.StatusOK, signed)
}

func (h *Handler) completeRecordingPackage(w http.ResponseWriter, r *http.Request) {
	if !h.recordingPackageAllowed(w, r) {
		return
	}
	p, err := h.recordingPackages.Complete(r.Context(), r.PathValue("packageID"), strings.TrimSpace(r.Header.Get("X-HHC-Actor-ID")))
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusAccepted, p)
}
