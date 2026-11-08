package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"hhc/asset-api/internal/assets"
)

func (h *Handler) recordingPackageAllowed(w http.ResponseWriter, r *http.Request) bool {
	if callerFromRequest(r, h.allowDevCallerHeader) != "hhc-web-api" {
		writeError(w, http.StatusForbidden, "AST_FORBIDDEN", "caller cannot manage recording packages")
		return false
	}
	if h.recordingPackages == nil {
		writeError(w, http.StatusServiceUnavailable, "AST_UNAVAILABLE", "recording packages are not enabled")
		return false
	}
	return true
}

func (h *Handler) issueRecordingPackageGrant(w http.ResponseWriter, r *http.Request) {
	if !h.recordingPackageAllowed(w, r) {
		return
	}
	if h.recordingSigner == nil {
		writeError(w, http.StatusServiceUnavailable, "AST_UNAVAILABLE", "recording grants are unavailable")
		return
	}
	var input struct {
		RecordingID        string    `json:"recordingId"`
		ScopeID            string    `json:"scopeId"`
		RecordingExpiresAt time.Time `json:"recordingExpiresAt"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	var grant assets.RecordingGrant
	err := h.recordingPackages.WithReady(r.Context(), r.PathValue("packageID"), input.RecordingID, func(ctx context.Context, p assets.RecordingPackage, broadcasts assets.RecordingBroadcastReader) error {
		var err error
		if h.recordingBroadcasts != nil {
			policy, readErr := broadcasts.GetBroadcastRange(ctx, p.ID)
			if readErr != nil {
				return readErr
			}
			if policy != nil {
				projection, readErr := broadcasts.GetBroadcastProjection(ctx, p.ID)
				if readErr != nil {
					return readErr
				}
				if policy.MemberState != "vod" || policy.Revoked || policy.EndSequenceExclusive == nil || projection.State != "ready" {
					return assets.ErrConflict
				}
				grant, err = h.recordingSigner.IssueBroadcastPackage(p, input.ScopeID, input.RecordingExpiresAt, time.Now(), *policy)
				return err
			}
		}
		grant, err = h.recordingSigner.IssuePackage(p, input.ScopeID, input.RecordingExpiresAt, time.Now())
		return err
	})
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	writeJSON(w, http.StatusOK, grant)
}

func (h *Handler) createRecordingPackage(w http.ResponseWriter, r *http.Request) {
	if !h.recordingPackageAllowed(w, r) {
		return
	}
	var input struct {
		RecordingID string          `json:"recordingId"`
		Inventory   json.RawMessage `json:"inventory"`
	}
	if !decodeJSONLimit(w, r, &input, assets.RecordingInventoryMaxBytes+1024) {
		return
	}
	inv, err := assets.DecodeRecordingInventory(bytes.NewReader(input.Inventory))
	if err != nil {
		handleError(w, err)
		return
	}
	p, err := h.recordingPackages.Create(r.Context(), assets.CreateRecordingPackageInput{
		ActorID: strings.TrimSpace(r.Header.Get("X-HHC-Actor-ID")), IdempotencyKey: strings.TrimSpace(r.Header.Get("Idempotency-Key")), RecordingID: input.RecordingID, Inventory: inv,
	})
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) getRecordingPackage(w http.ResponseWriter, r *http.Request) {
	if !h.recordingPackageAllowed(w, r) {
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil {
			handleError(w, assets.ErrInvalidInput)
			return
		}
	}
	page, err := h.recordingPackages.Status(r.Context(), r.PathValue("packageID"), strings.TrimSpace(r.Header.Get("X-HHC-Actor-ID")), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, page)
}

// CMS checks its own publication/viewer policy. This bounded private projection
// verifies the recording relationship without impersonating the uploader.
func (h *Handler) getReadyRecordingPackage(w http.ResponseWriter, r *http.Request) {
	if !h.recordingPackageAllowed(w, r) {
		return
	}
	p, err := h.recordingPackages.GetReady(r.Context(), r.PathValue("packageID"), r.URL.Query().Get("recordingId"))
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusOK, readyPackageProjection(p))
}

func readyPackageProjection(p assets.RecordingPackage) assets.RecordingPackageStatus {
	return assets.RecordingPackageStatus{Audio: p.Audio, PackageID: p.ID, SessionID: p.SessionID, RecordingID: p.RecordingID, State: p.State, SizeBytes: p.SizeBytes, ExpiresAt: p.ExpiresAt, ReadyAt: p.ReadyAt, MediaExpiresAt: p.MediaExpiresAt, UploadedAt: p.UploadedAt, RetentionRevision: p.RetentionRevision, Renditions: p.Inventory.Renditions, ConfirmedObjects: []string{}}
}

func (h *Handler) readyRecordingPackages(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	if !h.recordingPackageAllowed(w, r) {
		return
	}
	var input struct {
		Items []assets.RecordingLifecycleBinding `json:"items"`
	}
	if !decodeJSONLimit(w, r, &input, 32<<10) {
		return
	}
	if len(input.Items) < 1 || len(input.Items) > 100 {
		handleError(w, assets.ErrInvalidInput)
		return
	}
	seen := make(map[assets.RecordingLifecycleBinding]bool, len(input.Items))
	for _, binding := range input.Items {
		if binding.RecordingID == "" || len(binding.RecordingID) > 128 || strings.TrimSpace(binding.RecordingID) != binding.RecordingID || binding.PackageID == "" || len(binding.PackageID) > 128 || strings.TrimSpace(binding.PackageID) != binding.PackageID || seen[binding] {
			handleError(w, assets.ErrInvalidInput)
			return
		}
		seen[binding] = true
	}
	type result struct {
		assets.RecordingLifecycleBinding
		Status  string                         `json:"status"`
		Package *assets.RecordingPackageStatus `json:"package,omitempty"`
	}
	items := make([]result, 0, len(input.Items))
	for _, binding := range input.Items {
		if err := r.Context().Err(); err != nil {
			handleError(w, err)
			return
		}
		p, err := h.recordingPackages.GetReady(r.Context(), binding.PackageID, binding.RecordingID)
		item := result{RecordingLifecycleBinding: binding}
		switch {
		case err == nil:
			projection := readyPackageProjection(p)
			item.Status, item.Package = "ready", &projection
		case errors.Is(err, assets.ErrNotFound):
			item.Status = "not_found"
		case errors.Is(err, assets.ErrForbidden):
			item.Status = "forbidden"
		default:
			handleError(w, err)
			return
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, struct {
		Items []result `json:"items"`
	}{items})
}

func (h *Handler) signRecordingPackage(w http.ResponseWriter, r *http.Request) {
	if !h.recordingPackageAllowed(w, r) {
		return
	}
	var input struct {
		Paths []string `json:"paths"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	signed, err := h.recordingPackages.Sign(r.Context(), r.PathValue("packageID"), strings.TrimSpace(r.Header.Get("X-HHC-Actor-ID")), input.Paths)
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	writeJSON(w, http.StatusOK, signed)
}

func (h *Handler) completeRecordingPackage(w http.ResponseWriter, r *http.Request) {
	if !h.recordingPackageAllowed(w, r) {
		return
	}
	p, err := h.recordingPackages.Complete(r.Context(), r.PathValue("packageID"), strings.TrimSpace(r.Header.Get("X-HHC-Actor-ID")))
	if err != nil {
		handleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, http.StatusAccepted, p)
}
