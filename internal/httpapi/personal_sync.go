package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

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
		personalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) createPersonalUpload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	var input assets.PersonalUploadInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.SizeBytes > assets.PersonalMaxFileSize {
		writeError(w, http.StatusRequestEntityTooLarge, "AST_TOO_LARGE", "file exceeds 200 MiB")
		return
	}
	state, err := h.service.CreatePersonalUpload(r.Context(), collectionReaderSubject(r).UserID, input, r.Header.Get("Idempotency-Key"))
	if err != nil {
		personalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, state)
}
func (h *Handler) personalUploadStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	if !requireOpaqueID(w, r.PathValue("uploadID"), "upload ID") {
		return
	}
	state, err := h.service.PersonalUpload(r.Context(), collectionReaderSubject(r).UserID, r.PathValue("uploadID"))
	if err != nil {
		personalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}
func personalTransferContext(w http.ResponseWriter, r *http.Request) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(5 * time.Minute)
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
	return context.WithDeadline(r.Context(), deadline)
}
func (h *Handler) putPersonalUpload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	if !requireOpaqueID(w, r.PathValue("uploadID"), "upload ID") {
		return
	}
	if r.ContentLength > assets.PersonalMaxFileSize {
		writeError(w, http.StatusRequestEntityTooLarge, "AST_TOO_LARGE", "file exceeds 200 MiB")
		return
	}
	ctx, cancel := personalTransferContext(w, r)
	defer cancel()
	err := h.service.PutPersonalUpload(ctx, collectionReaderSubject(r).UserID, r.PathValue("uploadID"), r.ContentLength, http.MaxBytesReader(w, r.Body, assets.PersonalMaxFileSize))
	if err != nil {
		personalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (h *Handler) completePersonalUpload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	if !requireOpaqueID(w, r.PathValue("uploadID"), "upload ID") {
		return
	}
	var input assets.CompleteUploadInput
	if !decodeJSON(w, r, &input) {
		return
	}
	ctx, cancel := personalTransferContext(w, r)
	defer cancel()
	state, err := h.service.CompletePersonalUpload(ctx, collectionReaderSubject(r).UserID, r.PathValue("uploadID"), input)
	if err != nil {
		personalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}
func personalError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "AST_TOO_LARGE", "file exceeds upload limit")
		return
	}
	if errors.Is(err, assets.ErrInvalidUpload) {
		writeError(w, http.StatusUnprocessableEntity, "AST_INVALID_UPLOAD", "content failed validation")
		return
	}
	handleError(w, err)
}
func (h *Handler) personalContent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	if !requireOpaqueID(w, r.PathValue("itemID"), "item ID") {
		return
	}
	var revision int64
	if value := r.URL.Query().Get("revision"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 {
			handleError(w, assets.ErrInvalidInput)
			return
		}
		revision = parsed
	}
	ctx, cancel := personalTransferContext(w, r)
	defer cancel()
	metadata, err := h.service.PersonalContentMetadata(ctx, collectionReaderSubject(r).UserID, r.PathValue("itemID"), revision)
	if err != nil {
		personalError(w, err)
		return
	}
	h.serveCollectionDownload(w, r.WithContext(ctx), metadata)
}
