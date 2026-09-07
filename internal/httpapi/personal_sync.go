package httpapi

import (
	"errors"
	"net/http"

	"hhc/asset-api/internal/assets"
)

func (h *Handler) ensurePersonalSpace(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	space, err := h.service.EnsurePersonalSpace(r.Context(), collectionReaderSubject(r).UserID)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, space)
}
func (h *Handler) personalChanges(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	limit, ok := collectionListLimit(w, r)
	if !ok {
		return
	}
	page, err := h.service.PersonalChanges(r.Context(), collectionReaderSubject(r).UserID, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}
func (h *Handler) personalMutation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	var input assets.PersonalMutation
	if !decodeJSON(w, r, &input) {
		return
	}
	result, err := h.service.ApplyPersonalMutation(r.Context(), collectionReaderSubject(r).UserID, input)
	if errors.Is(err, assets.ErrPersonalAssetNotReady) {
		writeError(w, http.StatusConflict, "asset-not-ready", "upload validation or scanning is not complete")
		return
	}
	if errors.Is(err, assets.ErrConflict) {
		writeError(w, http.StatusConflict, "AST_CONFLICT", "revision, name or operation conflicts with existing state")
		return
	}
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
