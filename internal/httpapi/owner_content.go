package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"slices"

	"hhc/asset-api/internal/assets"
)

func (h *Handler) ownerContent(w http.ResponseWriter, r *http.Request) {
	principal, _ := r.Context().Value(workloadPrincipalKey{}).(string)
	caller := authenticatedCaller(r)
	if caller != "hhc-web-api" || principal == "" || h.workloadAuth == nil ||
		!slices.Contains(h.workloadAuth.config.OwnerContentPrincipals, principal) {
		handleError(w, assets.ErrForbidden)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) != 3 || len(query["sha256"]) != 1 ||
		len(query["ownerType"]) != 1 || len(query["ownerId"]) != 1 ||
		!validOpaqueID(query.Get("ownerType")) || !validOpaqueID(query.Get("ownerId")) {
		handleError(w, assets.ErrInvalidInput)
		return
	}
	input := assets.OwnerContentInput{SHA256: query.Get("sha256"), OwnerType: query.Get("ownerType"), OwnerID: query.Get("ownerId")}
	assetID := r.PathValue("assetID")
	metadata, err := h.service.OwnerContentMetadata(r.Context(), caller, assetID, input)
	if err != nil {
		handleError(w, err)
		return
	}
	h.serveBoundDownload(w, r, metadata, func(ctx context.Context, expected assets.PublicDownloadMetadata, byteRange assets.ByteRange) (assets.BlobDownload, error) {
		return h.service.OpenOwnerContent(ctx, caller, assetID, input, expected, byteRange)
	})
}
