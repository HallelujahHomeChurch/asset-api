package httpapi

import (
	"hhc/asset-api/internal/assets"
	"net/http"
)

func (h *Handler) completeBulletinDerivative(w http.ResponseWriter, r *http.Request) {
	// Dapr or Entra workload: either way the exact worker service identity.
	if requestService(r) != "hhc-web-bulletin-worker" {
		writeError(w, http.StatusForbidden, "AST_FORBIDDEN", "watermark worker identity is required")
		return
	}
	var input assets.CompleteDerivedBulletinInput
	if !decodeJSON(w, r, &input) || !h.requireOwnedAsset(w, r) {
		return
	}
	asset, err := h.service.CompleteDerivedBulletin(r.Context(), r.PathValue("assetID"), input)
	if err != nil {
		handleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, asset)
}
