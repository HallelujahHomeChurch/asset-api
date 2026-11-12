package httpapi

import (
	"net/http"
	"slices"

	"hhc/asset-api/internal/assets"
)

// uploadPolicy is preflight metadata, never an upload or content authorization.
func (h *Handler) uploadPolicy(w http.ResponseWriter, r *http.Request) {
	principal, _ := r.Context().Value(workloadPrincipalKey{}).(string)
	if principal == "" {
		handleError(w, assets.ErrForbidden)
		return
	}
	var input struct {
		Namespace string `json:"namespace"`
		assets.UploadProfiles
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	policy, ok := assets.UploadPolicy(input.Namespace, input.UploadProfiles)
	if !ok || input.SecurityProfile == "" || input.ProcessingProfile == "" {
		handleError(w, assets.ErrInvalidInput)
		return
	}
	if policy.OwnerService != authenticatedCaller(r) || !h.uploadProfilesAllowed(r, assets.CreateUploadInput{Namespace: input.Namespace, UploadProfiles: input.UploadProfiles}) {
		handleError(w, assets.ErrForbidden)
		return
	}
	mimes := make([]string, 0, len(policy.MIMETypes))
	for mime, allowed := range policy.MIMETypes {
		if allowed {
			mimes = append(mimes, mime)
		}
	}
	slices.Sort(mimes)
	visibilities := make([]assets.Visibility, 0, len(policy.Visibilities))
	for visibility, allowed := range policy.Visibilities {
		if allowed {
			visibilities = append(visibilities, visibility)
		}
	}
	slices.Sort(visibilities)
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, struct {
		Version   int    `json:"version"`
		Namespace string `json:"namespace"`
		assets.UploadProfiles
		ScanRequired     bool                `json:"scanRequired"`
		ReadinessVersion int                 `json:"readinessVersion"`
		MIMETypes        []string            `json:"mimeTypes"`
		MaxSizeBytes     int64               `json:"maxSizeBytes"`
		MIMEMaxSizeBytes map[string]int64    `json:"mimeMaxSizeBytes,omitempty"`
		Visibilities     []assets.Visibility `json:"visibilities"`
	}{1, input.Namespace, input.UploadProfiles, true, 1, mimes, policy.MaxSizeBytes, policy.MIMEMaxSizeBytes, visibilities})
}
