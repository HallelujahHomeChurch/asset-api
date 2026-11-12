package httpapi

import (
	"hhc/asset-api/internal/assets"
	"net/http"
	"strconv"
	"strings"
)

func (h *Handler) sourceContent(w http.ResponseWriter, r *http.Request) {
	metadata, err := h.service.LineSourceContentMetadata(r.Context(), r.PathValue("assetID"), authenticatedCaller(r))
	if err != nil {
		handleError(w, err)
		return
	}
	h.serveCollectionDownload(w, r, metadata)
}

func writeSourceError(w http.ResponseWriter, err *assets.SourceAccessError) {
	status := http.StatusServiceUnavailable
	switch err.Code {
	case "AST_SOURCE_PAUSED":
		status = http.StatusLocked
	case "AST_SOURCE_EXPIRED", "AST_SOURCE_REVOKED":
		status = http.StatusGone
	case "AST_SOURCE_ACL_REVOKED":
		status = http.StatusForbidden
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": err.Code, "scope": err.Scope, "message": "source access is unavailable"}})
}

func (h *Handler) collectionControl(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("collectionID")
	if !requireOpaqueID(w, id, "collection ID") {
		return
	}
	control, err := h.service.CollectionControl(r.Context(), id, collectionReaderSubject(r))
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, control)
}

func (h *Handler) stageSourceItem(w http.ResponseWriter, r *http.Request) {
	caller, key, ok := collectionMutationIdentity(w, r)
	id := r.PathValue("collectionID")
	if !ok || !requireOpaqueID(w, id, "collection ID") {
		return
	}
	var input assets.StageSourceItemInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.CollectionID, input.CallerService, input.IdempotencyKey = id, caller, key
	result, err := h.service.StageSourceItem(r.Context(), input)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}
func (h *Handler) activateSourceItem(w http.ResponseWriter, r *http.Request) {
	caller, key, ok := collectionMutationIdentity(w, r)
	id, item := r.PathValue("collectionID"), r.PathValue("itemID")
	if !ok || !requireOpaqueID(w, id, "collection ID") || !requireOpaqueID(w, item, "item ID") {
		return
	}
	var input assets.ActivateSourceItemInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.CollectionID, input.ItemID, input.CallerService, input.IdempotencyKey = id, item, caller, key
	result, err := h.service.ActivateSourceItem(r.Context(), input)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (h *Handler) getSourcePublication(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("collectionID")
	if !requireOpaqueID(w, id, "collection ID") {
		return
	}
	result, err := h.service.GetSourcePublication(r.Context(), id, r.PathValue("operationID"), authenticatedCaller(r))
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, result)
}
func sourceACLRevision(w http.ResponseWriter, r *http.Request) (int64, bool) {
	values := r.Header.Values("If-Match")
	if len(values) == 0 && authenticatedCaller(r) != assets.LineHarnessCaller {
		return 0, true
	}
	if len(values) == 1 {
		value := values[0]
		revision, err := strconv.ParseInt(strings.Trim(value, `"`), 10, 64)
		if err == nil && revision > 0 && value == strconv.Quote(strconv.FormatInt(revision, 10)) {
			return revision, true
		}
	}
	writeError(w, http.StatusBadRequest, "AST_INVALID_REQUEST", "an exact ACL revision is required")
	return 0, false
}

func (h *Handler) bindSourceCollection(w http.ResponseWriter, r *http.Request) {
	caller, key, ok := collectionMutationIdentity(w, r)
	id := r.PathValue("collectionID")
	if !ok || !requireOpaqueID(w, id, "collection ID") {
		return
	}
	var input assets.BindSourceCollectionInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.CollectionID, input.CallerService, input.IdempotencyKey = id, caller, key
	result, err := h.service.BindSourceCollection(r.Context(), input)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) getSourceUpload(w http.ResponseWriter, r *http.Request) {
	result, err := h.service.GetSourceUpload(r.Context(), r.PathValue("operationID"), authenticatedCaller(r))
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, result)
}
