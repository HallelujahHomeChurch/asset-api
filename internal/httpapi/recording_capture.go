package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"hhc/asset-api/internal/assets"
	"net/http"
	"strconv"
	"strings"
)

func (h *Handler) WithRecordingCaptures(service *assets.RecordingCaptureService) *Handler {
	h.recordingCaptures = service
	return h
}

// Reuse private authentication and bounded JSON parsing; expose the capture
// error vocabulary on these routes, including middleware rejection.
type captureResponseWriter struct{ http.ResponseWriter }

func (w captureResponseWriter) Write(data []byte) (int, error) {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &body) == nil && strings.HasPrefix(body.Error.Code, "AST_") {
		codes := map[string]string{"AST_FORBIDDEN": "capture_forbidden", "AST_UNAUTHORIZED": "capture_unauthorized", "AST_INVALID_INPUT": "capture_invalid", "AST_INVALID_REQUEST": "capture_invalid", "AST_NOT_FOUND": "capture_not_found", "AST_CONFLICT": "capture_conflict"}
		code := codes[body.Error.Code]
		if code == "" {
			code = "capture_unavailable"
		}
		body.Error.Code = code
		out, _ := json.Marshal(body)
		_, err := w.ResponseWriter.Write(append(out, '\n'))
		return len(data), err
	}
	return w.ResponseWriter.Write(data)
}
func (h *Handler) captureInternal(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.internal(next).ServeHTTP(captureResponseWriter{w}, r) })
}
func (h *Handler) captureAllowed(w http.ResponseWriter, r *http.Request) bool {
	if callerFromRequest(r, h.allowDevCallerHeader) != "hhc-web-api" {
		writeError(w, 403, "capture_forbidden", "caller cannot manage captures")
		return false
	}
	if h.recordingCaptures == nil {
		writeError(w, 503, "capture_unavailable", "recording captures are not enabled")
		return false
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	return true
}
func captureError(w http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, "capture_unavailable"
	switch {
	case errors.Is(err, assets.ErrInvalidInput):
		status, code = 400, "capture_invalid"
	case errors.Is(err, assets.ErrForbidden):
		status, code = 403, "capture_forbidden"
	case errors.Is(err, assets.ErrNotFound):
		status, code = 404, "capture_not_found"
	case errors.Is(err, assets.ErrCaptureExpired):
		status, code = 410, "capture_expired"
	case errors.Is(err, assets.ErrCaptureMissingObjects):
		status, code = 409, "capture_missing_objects"
	case errors.Is(err, assets.ErrConflict):
		status, code = 409, "capture_conflict"
	case errors.Is(err, assets.ErrRecordingPackageTooLarge):
		status, code = 413, "capture_too_large"
	}
	writeError(w, status, code, code)
}
func captureActor(r *http.Request) string { return strings.TrimSpace(r.Header.Get("X-HHC-Actor-ID")) }
func (h *Handler) createRecordingCapture(w http.ResponseWriter, r *http.Request) {
	if !h.captureAllowed(w, r) {
		return
	}
	var input struct {
		RecordingID  string `json:"recordingId"`
		OperationKey string `json:"operationKey"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	result, err := h.recordingCaptures.Create(r.Context(), input.RecordingID, captureActor(r), input.OperationKey)
	if err != nil {
		captureError(w, err)
		return
	}
	writeJSON(w, 201, result)
}
func (h *Handler) getRecordingCapture(w http.ResponseWriter, r *http.Request) {
	if !h.captureAllowed(w, r) {
		return
	}
	limit := 1000
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil {
			captureError(w, assets.ErrInvalidInput)
			return
		}
	}
	result, err := h.recordingCaptures.Get(r.Context(), r.PathValue("captureID"), captureActor(r), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		captureError(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func (h *Handler) declareRecordingCapture(w http.ResponseWriter, r *http.Request) {
	if !h.captureAllowed(w, r) {
		return
	}
	var input struct {
		OperationKey string                          `json:"operationKey"`
		Objects      []assets.RecordingPackageObject `json:"objects"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	result, err := h.recordingCaptures.Declare(r.Context(), r.PathValue("captureID"), captureActor(r), input.OperationKey, input.Objects)
	if err != nil {
		captureError(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func (h *Handler) signRecordingCapture(w http.ResponseWriter, r *http.Request) {
	if !h.captureAllowed(w, r) {
		return
	}
	var input struct {
		Paths []string `json:"paths"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	result, err := h.recordingCaptures.Sign(r.Context(), r.PathValue("captureID"), captureActor(r), input.Paths)
	if err != nil {
		captureError(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func (h *Handler) confirmRecordingCapture(w http.ResponseWriter, r *http.Request) {
	if !h.captureAllowed(w, r) {
		return
	}
	var input struct {
		OperationKey string   `json:"operationKey"`
		Paths        []string `json:"paths"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	result, err := h.recordingCaptures.Confirm(r.Context(), r.PathValue("captureID"), captureActor(r), input.OperationKey, input.Paths)
	if err != nil {
		captureError(w, err)
		return
	}
	writeJSON(w, 202, result)
}
func (h *Handler) sealRecordingCapture(w http.ResponseWriter, r *http.Request) {
	if !h.captureAllowed(w, r) {
		return
	}
	var input struct {
		OperationKey string          `json:"operationKey"`
		NormalEnd    bool            `json:"normalEnd"`
		Inventory    json.RawMessage `json:"inventory"`
	}
	if !decodeJSONLimit(w, r, &input, assets.RecordingInventoryMaxBytes+1024) {
		return
	}
	inv, err := assets.DecodeRecordingInventory(bytes.NewReader(input.Inventory))
	if err != nil {
		captureError(w, err)
		return
	}
	result, err := h.recordingCaptures.Seal(r.Context(), r.PathValue("captureID"), captureActor(r), input.OperationKey, input.NormalEnd, inv)
	if err != nil {
		captureError(w, err)
		return
	}
	writeJSON(w, 202, result)
}
func (h *Handler) abortRecordingCapture(w http.ResponseWriter, r *http.Request) {
	if !h.captureAllowed(w, r) {
		return
	}
	var input struct {
		OperationKey string `json:"operationKey"`
		ReasonCode   string `json:"reasonCode"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	result, err := h.recordingCaptures.Abort(r.Context(), r.PathValue("captureID"), captureActor(r), input.OperationKey, input.ReasonCode)
	if err != nil {
		captureError(w, err)
		return
	}
	writeJSON(w, 200, result)
}
func (h *Handler) unavailableLiveCapture(w http.ResponseWriter, r *http.Request) {
	if !h.captureAllowed(w, r) {
		return
	}
	writeError(w, 503, "capture_unavailable", "immutable progressive media validation is not enabled")
}
