package httpapi

import "net/http"

func (h *Handler) createPersonalFolderShare(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	item := r.PathValue("itemID")
	if !requireOpaqueID(w, item, "item ID") {
		return
	}
	var input struct {
		GranteeUserID string `json:"granteeUserId"`
	}
	if !decodeJSON(w, r, &input) || !requireOpaqueID(w, input.GranteeUserID, "grantee user ID") {
		return
	}
	grant, err := h.service.CreatePersonalFolderGrant(r.Context(), collectionReaderSubject(r).UserID, item, input.GranteeUserID)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, grant)
}

func (h *Handler) listPersonalFolderShares(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	item := r.PathValue("itemID")
	if !requireOpaqueID(w, item, "item ID") {
		return
	}
	grants, err := h.service.ListPersonalFolderGrants(r.Context(), collectionReaderSubject(r).UserID, item)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": grants})
}

func (h *Handler) revokePersonalFolderShare(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	item, grant := r.PathValue("itemID"), r.PathValue("grantID")
	if !requireOpaqueID(w, item, "item ID") || !requireOpaqueID(w, grant, "grant ID") {
		return
	}
	if err := h.service.RevokePersonalFolderGrant(r.Context(), collectionReaderSubject(r).UserID, item, grant); err != nil {
		handleError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listSharedFolders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	roots, err := h.service.ListSharedFolderRoots(r.Context(), collectionReaderSubject(r).UserID)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"folders": roots})
}

func (h *Handler) sharedFolderSnapshot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	grant := r.PathValue("grantID")
	if !requireOpaqueID(w, grant, "grant ID") {
		return
	}
	limit, ok := collectionListLimit(w, r)
	if !ok {
		return
	}
	page, err := h.service.SharedFolderSnapshot(r.Context(), collectionReaderSubject(r).UserID, grant, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) sharedFolderContent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	grant, item := r.PathValue("grantID"), r.PathValue("itemID")
	if !requireOpaqueID(w, grant, "grant ID") || !requireOpaqueID(w, item, "item ID") {
		return
	}
	ctx, cancel := personalTransferContext(w, r)
	defer cancel()
	metadata, err := h.service.SharedFolderContentMetadata(ctx, collectionReaderSubject(r).UserID, grant, item)
	if err != nil {
		handleError(w, err)
		return
	}
	h.serveCollectionDownload(w, r.WithContext(ctx), metadata)
}

func (h *Handler) leaveSharedFolder(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	grant := r.PathValue("grantID")
	if !requireOpaqueID(w, grant, "grant ID") {
		return
	}
	if err := h.service.LeaveSharedFolder(r.Context(), collectionReaderSubject(r).UserID, grant); err != nil {
		handleError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
