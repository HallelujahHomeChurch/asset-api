package httpapi

import (
	"net/http"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/config"
)

const extractorCaller = config.BulletinExtractorCaller

func extractorRouteAllowed(r *http.Request) bool {
	switch r.Pattern {
	case "POST /priv/assets/upload-sessions", "POST /priv/assets/{assetID}/complete",
		"POST /priv/assets/{assetID}/grants", "DELETE /priv/assets/{assetID}/grants/{grantID}":
		return true
	case "GET /priv/assets/{assetID}":
		return r.Method == http.MethodGet
	case "GET /priv/assets/{assetID}/{action}":
		return r.Method == http.MethodGet && r.PathValue("action") == "download"
	default:
		return false
	}
}

func extractorIllustration(namespace, ownerType, ownerID, purpose, locale, mime string, visibility assets.Visibility) bool {
	return namespace == "cms.weekly.image" && ownerType == "bulletin_issue" && ownerID != "" &&
		purpose == "online_bulletin_illustration" && locale == "zh-Hant" && mime == "image/png" && visibility == assets.VisibilityPrivate
}

func extractorAssetAllowed(r *http.Request, asset assets.Asset) bool {
	if asset.OwnerService != "hhc-web-api" || !asset.PurgedAt.IsZero() {
		return false
	}
	source := asset.Namespace == "cms.weekly.pdf" && asset.OwnerType == "bulletin_issue" && asset.OwnerID != "" &&
		asset.Purpose == "weekly_bulletin" && asset.Locale == "zh-Hant" && asset.Visibility == assets.VisibilityPrivate
	illustration := extractorIllustration(asset.Namespace, asset.OwnerType, asset.OwnerID, asset.Purpose,
		asset.Locale, asset.ExpectedMIMEType, asset.Visibility)
	switch r.Pattern {
	case "GET /priv/assets/{assetID}":
		return source || illustration
	case "POST /priv/assets/{assetID}/complete":
		return illustration
	case "DELETE /priv/assets/{assetID}/grants/{grantID}":
		return source
	default:
		return source && asset.UploadStatus == assets.UploadCompleted && asset.ScanStatus == assets.ScanClean &&
			(asset.ProcessingStatus == assets.ProcessingNotRequired || asset.ProcessingStatus == assets.ProcessingReady) &&
			asset.DetectedMIMEType == "application/pdf" && asset.ETag != ""
	}
}

func extractorGrantAllowed(input assets.CreateGrantInput, now time.Time) bool {
	return input.SubjectType == assets.SubjectService && input.SubjectID == extractorCaller &&
		input.Permission == assets.PermissionRead && input.ExpiresAt.After(now) &&
		!input.ExpiresAt.After(now.Add(time.Hour))
}

func extractorDownloadAllowed(r *http.Request) bool {
	subject, id := r.Header.Values("X-Asset-Subject-Type"), r.Header.Values("X-Asset-Subject-Id")
	return len(subject) == 1 && subject[0] == string(assets.SubjectService) && len(id) == 1 && id[0] == extractorCaller
}
