package httpapi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"hhc/asset-api/internal/assets"
	"io"
	"net/http"
)

type RecordingLiveCoverStore interface {
	Create(context.Context, string, string, string, string, string, string, string) (assets.RecordingLiveCover, error)
	Queue(context.Context, string) error
	Get(context.Context, string, string, string) (assets.RecordingLiveCover, error)
	Retain(context.Context, string, string, string, string) error
	Release(context.Context, string, string, string) error
	Promote(context.Context, string, string, string) (assets.RecordingCover, error)
}

func (h *Handler) WithRecordingLiveCovers(store RecordingLiveCoverStore) *Handler {
	h.recordingLiveCovers = store
	return h
}
func (h *Handler) liveCoverAllowed(w http.ResponseWriter, r *http.Request) bool {
	if !h.coverAllowed(w, r) {
		return false
	}
	if h.recordingLiveCovers == nil {
		writeError(w, 503, "AST_UNAVAILABLE", "live covers unavailable")
		return false
	}
	return true
}
func (h *Handler) uploadLiveCover(w http.ResponseWriter, r *http.Request) {
	if !h.liveCoverAllowed(w, r) {
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, assets.RecordingCoverInputMaxBytes))
	if err != nil {
		writeError(w, 413, "cover_too_large", "cover exceeds 5 MiB")
		return
	}
	mime := r.Header.Get("Content-Type")
	if _, err = assets.ValidateRecordingCoverInput(data, mime); err != nil {
		writeError(w, 422, "invalid_cover", "invalid JPEG/PNG")
		return
	}
	c, err := h.recordingLiveCovers.Create(r.Context(), r.PathValue("scope"), r.URL.Query().Get("recordingId"), r.Header.Get("X-HHC-Actor-ID"), r.Header.Get("Idempotency-Key"), mime, fmt.Sprintf("%x", sha256.Sum256(data)), "custom")
	if err != nil {
		handleError(w, err)
		return
	}
	if c.State == "uploading" {
		if err = h.coverObjects.PutCoverInput(r.Context(), c.InputKey(), data, mime); err != nil {
			writeError(w, 503, "AST_UNAVAILABLE", "live cover upload unavailable")
			return
		}
		if err = h.recordingLiveCovers.Queue(r.Context(), c.ID); err != nil {
			handleError(w, err)
			return
		}
		c.State = "pending"
	}
	writeJSON(w, 202, c)
}
func (h *Handler) generateLiveCover(w http.ResponseWriter, r *http.Request) {
	if !h.liveCoverAllowed(w, r) {
		return
	}
	c, err := h.recordingLiveCovers.Create(r.Context(), r.PathValue("scope"), r.URL.Query().Get("recordingId"), "system", "auto", "", "", "auto")
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, 202, c)
}
func (h *Handler) getLiveCover(w http.ResponseWriter, r *http.Request) {
	if !h.liveCoverAllowed(w, r) {
		return
	}
	c, err := h.recordingLiveCovers.Get(r.Context(), r.PathValue("uploadID"), r.PathValue("scope"), r.URL.Query().Get("recordingId"))
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, 200, c)
}
func (h *Handler) readLiveCover(w http.ResponseWriter, r *http.Request) {
	if !h.liveCoverAllowed(w, r) {
		return
	}
	c, err := h.recordingLiveCovers.Get(r.Context(), r.PathValue("uploadID"), r.PathValue("scope"), r.URL.Query().Get("recordingId"))
	if err != nil {
		handleError(w, err)
		return
	}
	if c.State != "ready" {
		handleError(w, assets.ErrNotFound)
		return
	}
	body, err := h.coverObjects.Open(r.Context(), c.Key())
	if err != nil {
		writeError(w, 503, "AST_UNAVAILABLE", "cover unavailable")
		return
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, assets.RecordingCoverOutputMaxBytes+1))
	if err != nil || len(data) > assets.RecordingCoverOutputMaxBytes || fmt.Sprintf("%x", sha256.Sum256(data)) != c.OutputDigest {
		writeError(w, 503, "AST_UNAVAILABLE", "cover unavailable")
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(data)
}
func (h *Handler) retainLiveCover(w http.ResponseWriter, r *http.Request) {
	if !h.liveCoverAllowed(w, r) {
		return
	}
	var input struct {
		ReferenceID string `json:"referenceId"`
	}
	if !decodeJSONLimit(w, r, &input, 2048) {
		return
	}
	if err := h.recordingLiveCovers.Retain(r.Context(), r.PathValue("uploadID"), r.PathValue("scope"), r.URL.Query().Get("recordingId"), input.ReferenceID); err != nil {
		handleError(w, err)
		return
	}
	w.WriteHeader(204)
}
func (h *Handler) releaseLiveCover(w http.ResponseWriter, r *http.Request) {
	if !h.liveCoverAllowed(w, r) {
		return
	}
	if err := h.recordingLiveCovers.Release(r.Context(), r.PathValue("uploadID"), r.PathValue("scope"), r.PathValue("referenceID")); err != nil {
		handleError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (h *Handler) promoteLiveCover(w http.ResponseWriter, r *http.Request) {
	if !h.liveCoverAllowed(w, r) {
		return
	}
	var c assets.RecordingCover
	var err error
	if r.ContentLength != 0 {
		var input struct {
			TargetCaptureID string `json:"targetCaptureId"`
			RecordingID     string `json:"recordingId"`
		}
		if !decodeJSONLimit(w, r, &input, 2048) {
			return
		}
		if h.recordingBroadcasts == nil {
			writeError(w, 503, "AST_NOT_READY", "broadcast projection unavailable")
			return
		}
		policy, readErr := h.recordingBroadcasts.GetBroadcastRange(r.Context(), input.TargetCaptureID)
		if readErr != nil {
			handleError(w, readErr)
			return
		}
		if policy == nil || policy.RecordingID != input.RecordingID || policy.Revoked || policy.EndSequenceExclusive == nil {
			handleError(w, assets.ErrConflict)
			return
		}
		projection, readErr := h.recordingBroadcasts.GetBroadcastProjection(r.Context(), input.TargetCaptureID)
		if readErr != nil {
			handleError(w, readErr)
			return
		}
		if projection.State != "ready" {
			handleError(w, assets.ErrConflict)
			return
		}
		repo, ok := h.recordingLiveCovers.(interface {
			PromoteTo(context.Context, string, string, string, string, string) (assets.RecordingCover, error)
		})
		if !ok {
			writeError(w, 503, "AST_NOT_READY", "broadcast promotion unavailable")
			return
		}
		c, err = repo.PromoteTo(r.Context(), r.PathValue("uploadID"), r.PathValue("scope"), r.URL.Query().Get("recordingId"), input.TargetCaptureID, input.RecordingID)
	} else {
		c, err = h.recordingLiveCovers.Promote(r.Context(), r.PathValue("uploadID"), r.PathValue("scope"), r.URL.Query().Get("recordingId"))
	}
	if err != nil {
		handleError(w, err)
		return
	}
	index := 0
	if c.Kind == "live-auto" {
		index = 1
	}
	writeJSON(w, 202, map[string]string{"coverId": fmt.Sprintf("%s-%d", c.ID, index), "state": c.State})
}
