package httpapi

import (
	"context"
	"hhc/asset-api/internal/assets"
	"net/http"
)

type WebsiteContentReadinessStore interface {
	WebsiteContentReadiness(context.Context, []assets.WebsiteContentRef) ([]assets.WebsiteContentDecision, error)
}

func (h *Handler) WithWebsiteContentReadiness(store WebsiteContentReadinessStore, websiteAppIDs ...string) *Handler {
	h.websiteCallerAppID = "hhc-web-api"
	if len(websiteAppIDs) > 0 && websiteAppIDs[0] != "" {
		h.websiteCallerAppID = websiteAppIDs[0]
	}
	h.websiteReadiness = store
	return h
}
func (h *Handler) websiteContentReadiness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	if len(r.Header.Values("Dapr-Caller-App-Id")) != 1 || r.Header.Get("Dapr-Caller-App-Id") != h.websiteCallerAppID || h.daprCaller(r) != h.websiteCallerAppID || len(r.Header.Values("dapr-api-token")) > 1 {
		handleError(w, assets.ErrForbidden)
		return
	}
	if h.websiteReadiness == nil {
		writeError(w, 503, "AST_UNAVAILABLE", "content readiness unavailable")
		return
	}
	var in struct {
		Items []assets.WebsiteContentRef `json:"items"`
	}
	if !decodeJSONLimit(w, r, &in, 64<<10) {
		return
	}
	if err := assets.ValidateWebsiteContentRefs(in.Items); err != nil {
		handleError(w, err)
		return
	}
	for _, ref := range in.Items {
		if ref.Type == "recording_live" && h.recordingCaptures == nil {
			writeError(w, 503, "AST_UNAVAILABLE", "recording captures unavailable")
			return
		}
	}
	items, err := h.websiteReadiness.WebsiteContentReadiness(r.Context(), in.Items)
	if err != nil {
		writeError(w, 503, "AST_UNAVAILABLE", "content readiness unavailable")
		return
	}
	writeJSON(w, 200, struct {
		Items []assets.WebsiteContentDecision `json:"items"`
	}{items})
}
