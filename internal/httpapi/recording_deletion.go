package httpapi

import (
	"context"
	"net/http"
	"regexp"
)

type RecordingDeleter interface {
	DeleteRecording(context.Context, string) error
}

var recordingDeletionID = regexp.MustCompile(`^[a-zA-Z0-9-]{1,80}$`)

func (h *Handler) WithRecordingDeletion(store RecordingDeleter) *Handler {
	h.recordingDeletion = store
	return h
}

func (h *Handler) deleteRecording(w http.ResponseWriter, r *http.Request) {
	if !h.recordingPackageAllowed(w, r) {
		return
	}
	if h.recordingDeletion == nil {
		writeError(w, http.StatusServiceUnavailable, "AST_UNAVAILABLE", "recording deletion is unavailable")
		return
	}
	id := r.PathValue("recordingID")
	if !recordingDeletionID.MatchString(id) {
		writeError(w, http.StatusBadRequest, "AST_INVALID_INPUT", "invalid recording ID")
		return
	}
	if err := h.recordingDeletion.DeleteRecording(r.Context(), id); err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusNoContent)
}
