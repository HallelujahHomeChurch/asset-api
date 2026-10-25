package httpapi

import "net/http"

func (h *Handler) bulletinIllustrationDownload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	metadata, err := h.service.BulletinIllustrationMetadata(r.Context(), authenticatedCaller(r), r.URL.Query().Get("issueId"), r.PathValue("assetID"), r.URL.Query().Get("checksumSha256"))
	if err != nil {
		handleError(w, err)
		return
	}
	h.serveDownload(w, r, metadata)
}
