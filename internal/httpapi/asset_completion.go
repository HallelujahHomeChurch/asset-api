package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"

	"hhc/asset-api/internal/assets"
)

type completeAssetRequest struct {
	assets.CompleteUploadInput
	Derivation json.RawMessage `json:"derivation"`
}

func (h *Handler) completeUpload(w http.ResponseWriter, r *http.Request) {
	if !h.requireOwnedAsset(w, r) {
		return
	}
	var input completeAssetRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	assetID := r.PathValue("assetID")
	var asset assets.Asset
	var err error
	if input.Derivation == nil {
		asset, err = h.service.CompleteUpload(r.Context(), assetID, input.CompleteUploadInput)
	} else {
		principal, _ := r.Context().Value(workloadPrincipalKey{}).(string)
		if requestService(r) != "hhc-web-bulletin-worker" || principal == "" || h.workloadAuth == nil ||
			!slices.Contains(h.workloadAuth.config.TrustedDerivationPrincipals, principal) {
			handleError(w, assets.ErrForbidden)
			return
		}
		var proof assets.Derivation
		decoder := json.NewDecoder(bytes.NewReader(input.Derivation))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&proof); err != nil || !proof.Valid() || !validOpaqueID(proof.SourceAssetID) {
			handleError(w, assets.ErrInvalidInput)
			return
		}
		asset, err = h.service.CompleteDerivedUpload(r.Context(), assetID, input.CompleteUploadInput, proof)
	}
	if err != nil {
		handleError(w, err)
		return
	}
	writePrivateAsset(w, r, asset)
}

type assetReadiness struct {
	Version int    `json:"version"`
	State   string `json:"state"`
}

type assetSafetyEvidence struct {
	Kind string `json:"kind"`
}

func writePrivateAsset(w http.ResponseWriter, r *http.Request, asset assets.Asset) {
	principal, _ := r.Context().Value(workloadPrincipalKey{}).(string)
	if principal == "" {
		writeJSON(w, http.StatusOK, asset)
		return
	}
	readiness := assetReadiness{Version: 1, State: "rejected"}
	var evidence *assetSafetyEvidence
	if asset.DeletedAt.IsZero() && asset.PurgedAt.IsZero() {
		switch asset.UploadStatus {
		case assets.UploadCreated:
			if asset.ScanStatus == assets.ScanPending {
				readiness.State = "pending"
			}
		case assets.UploadCompleted:
			switch asset.ScanStatus {
			case assets.ScanPending:
				readiness.State = "pending"
			case assets.ScanClean:
				switch asset.ProcessingStatus {
				case assets.ProcessingReady, assets.ProcessingNotRequired:
					readiness.State = "ready"
				case assets.ProcessingPending:
					readiness.State = "pending"
				}
				evidence = &assetSafetyEvidence{Kind: "scanned"}
				if asset.ScanSignature == assets.TrustedBulletinWatermark {
					evidence.Kind = "trusted-derived"
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, struct {
		assets.Asset
		ProcessingReadiness assetReadiness       `json:"processingReadiness"`
		SafetyEvidence      *assetSafetyEvidence `json:"safetyEvidence,omitempty"`
	}{asset, readiness, evidence})
}
