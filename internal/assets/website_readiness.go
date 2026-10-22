package assets

import (
	"regexp"
	"time"
)

// Website owns audience/publication authority; Asset only verifies these bindings.
type WebsiteContentRef struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	ResourceID string `json:"resourceId"`
	Locale     string `json:"locale,omitempty"`
}
type WebsiteLiveReadiness struct {
	State      string                `json:"state"`
	CreatedAt  time.Time             `json:"createdAt"`
	ExpiresAt  time.Time             `json:"expiresAt"`
	TerminalAt *time.Time            `json:"terminalAt"`
	Progress   RecordingLiveProgress `json:"progress"`
}
type WebsiteContentDecision struct {
	Ref   WebsiteContentRef     `json:"ref"`
	Ready bool                  `json:"ready"`
	Live  *WebsiteLiveReadiness `json:"live,omitempty"`
}

var websiteContentID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

func ValidateWebsiteContentRefs(refs []WebsiteContentRef) error {
	if len(refs) < 1 || len(refs) > 100 {
		return ErrInvalidInput
	}
	seen := map[WebsiteContentRef]bool{}
	for _, ref := range refs {
		if !websiteContentID.MatchString(ref.ID) || !websiteContentID.MatchString(ref.ResourceID) || seen[ref] {
			return ErrInvalidInput
		}
		seen[ref] = true
		switch ref.Type {
		case "bulletin_pdf":
			if ref.Locale != "zh-Hant" && ref.Locale != "zh-Hans" && ref.Locale != "en" {
				return ErrInvalidInput
			}
		case "recording_live":
			if ref.Locale != "" {
				return ErrInvalidInput
			}
		default:
			return ErrInvalidInput
		}
	}
	return nil
}
