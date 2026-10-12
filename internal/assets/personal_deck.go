package assets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"strings"
)

type personalDeckElement struct {
	Adjustments json.RawMessage `json:"adjustments"`
	Shape       string          `json:"shape"`
	SourceNode  json.RawMessage `json:"pptxSourceNode"`
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	AssetID     string          `json:"assetId"`
	X           float64         `json:"x"`
	Y           float64         `json:"y"`
	Width       float64         `json:"width"`
	Height      float64         `json:"height"`
	Rotation    float64         `json:"rotation"`
	Opacity     float64         `json:"opacity"`
}

// ValidatePersonalDeck checks the portable document graph before it can become a cloud head.
// Legacy documents without a schema version use the same graph; clients migrate them to v1.
func ValidatePersonalDeck(ctx context.Context, reader io.Reader) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var deck struct {
		SchemaVersion *int                               `json:"schemaVersion"`
		ID            string                             `json:"id"`
		Name          string                             `json:"name"`
		Width         float64                            `json:"width"`
		Height        float64                            `json:"height"`
		SlideOrder    []string                           `json:"slideOrder"`
		Slides        map[string]personalDeckSourceSlide `json:"slides"`
		Source        json.RawMessage                    `json:"pptxSource"`
		Assets        map[string]struct {
			ID       string `json:"id"`
			MIMEType string `json:"mimeType"`
			DataURL  string `json:"dataUrl"`
		} `json:"assets"`
		Themes         map[string]json.RawMessage `json:"themes"`
		DefaultThemeID string                     `json:"defaultThemeId"`
	}
	limited := &io.LimitedReader{R: &contextReader{ctx: ctx, reader: reader}, N: PersonalMaxFileSize + 1}
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(&deck); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrInvalidUpload
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrInvalidUpload
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if limited.N <= 0 {
		return ErrInvalidUpload
	}
	if deck.SchemaVersion != nil && *deck.SchemaVersion != 1 && *deck.SchemaVersion != 2 && *deck.SchemaVersion != 3 {
		return ErrInvalidUpload
	}
	if strings.TrimSpace(deck.ID) == "" || strings.TrimSpace(deck.Name) == "" || !personalDimension(deck.Width) || !personalDimension(deck.Height) || deck.Slides == nil || len(deck.Slides) > 2000 || len(deck.Assets) > 10000 || len(deck.Themes) > 2000 || !personalOrder(deck.SlideOrder, deck.Slides) {
		return ErrInvalidUpload
	}
	if deck.DefaultThemeID != "" && deck.Themes[deck.DefaultThemeID] == nil {
		return ErrInvalidUpload
	}
	var source *personalDeckSource
	if deck.SchemaVersion == nil || *deck.SchemaVersion == 1 {
		if len(deck.Source) > 0 {
			return ErrInvalidUpload
		}
		for _, slide := range deck.Slides {
			if len(slide.SourcePart) > 0 {
				return ErrInvalidUpload
			}
			for _, element := range slide.Elements {
				if len(element.SourceNode) > 0 {
					return ErrInvalidUpload
				}
			}
		}
	} else if len(deck.Source) > 0 {
		if err := json.Unmarshal(deck.Source, &source); err != nil || source == nil {
			return ErrInvalidUpload
		}
	}
	total := 0
	for id, slide := range deck.Slides {
		if id != slide.ID || (slide.ThemeID != "" && deck.Themes[slide.ThemeID] == nil) || !personalOrder(slide.ElementOrder, slide.Elements) {
			return ErrInvalidUpload
		}
		total += len(slide.Elements)
		if total > 100000 || len(slide.Elements) > 10000 {
			return ErrInvalidUpload
		}
		for id, e := range slide.Elements {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if e.ID != id || !personalCoordinate(e.X) || !personalCoordinate(e.Y) || !personalCoordinate(e.Rotation) || math.IsNaN(e.Opacity) || e.Opacity < 0 || e.Opacity > 1 {
				return ErrInvalidUpload
			}
			if e.Type == "line" {
				if !personalCoordinate(e.Width) || !personalCoordinate(e.Height) {
					return ErrInvalidUpload
				}
			} else if !personalDimension(e.Width) || !personalDimension(e.Height) {
				return ErrInvalidUpload
			}
			if len(e.Adjustments) > 0 && (deck.SchemaVersion == nil || *deck.SchemaVersion != 3 || e.Type != "shape") {
				return ErrInvalidUpload
			}
			if e.Type == "shape" {
				count := personalPresetAdjustmentCount(e.Shape)
				if count < 0 || ((deck.SchemaVersion == nil || *deck.SchemaVersion != 3) && e.Shape != "rectangle" && e.Shape != "ellipse") {
					return ErrInvalidUpload
				}
				if len(e.Adjustments) > 0 {
					var values []*float64
					if err := json.Unmarshal(e.Adjustments, &values); err != nil || values == nil || len(values) > 16 || len(values) > count {
						return ErrInvalidUpload
					}
					for _, value := range values {
						if value == nil || math.Trunc(*value) != *value || *value < -2147483648 || *value > 2147483647 {
							return ErrInvalidUpload
						}
					}
				}
			}
			switch e.Type {
			case "image":
				if _, ok := deck.Assets[e.AssetID]; !ok {
					return ErrInvalidUpload
				}
			case "text", "shape", "line", "locked":
			default:
				return ErrInvalidUpload
			}
		}
	}
	for id, a := range deck.Assets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if id == "" || a.ID != id {
			return ErrInvalidUpload
		}
		extension := ""
		switch a.MIMEType {
		case "image/png":
			extension = ".png"
		case "image/jpeg":
			extension = ".jpg"
		case "image/gif":
			extension = ".gif"
		case "image/webp":
			extension = ".webp"
		case "image/bmp":
			extension = ".bmp"
		default:
			return ErrInvalidUpload
		}
		prefix := "data:" + a.MIMEType + ";base64,"
		if !strings.HasPrefix(a.DataURL, prefix) || len(a.DataURL)-len(prefix) > base64.StdEncoding.EncodedLen(20<<20) {
			return ErrInvalidUpload
		}
		data, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(a.DataURL, prefix))
		if err != nil || len(data) > 20<<20 {
			return ErrInvalidUpload
		}
		if _, err = ValidateMedia(ctx, "image"+extension, a.MIMEType, data, bytes.NewReader(data), int64(len(data))); err != nil {
			return ErrInvalidUpload
		}
	}
	return validatePersonalDeckSource(ctx, source, deck.Slides)
}
func personalDimension(n float64) bool  { return n > 0 && n <= 100000 && !math.IsNaN(n) }
func personalCoordinate(n float64) bool { return math.Abs(n) <= 1000000 && !math.IsNaN(n) }
func personalOrder[T any](order []string, items map[string]T) bool {
	if order == nil || len(order) != len(items) {
		return false
	}
	seen := make(map[string]bool, len(order))
	for _, id := range order {
		if id == "" || seen[id] {
			return false
		}
		if _, ok := items[id]; !ok {
			return false
		}
		seen[id] = true
	}
	return true
}

func personalPresetAdjustmentCount(shape string) int {
	switch shape {
	case "rectangle", "ellipse", "rtTriangle", "diamond":
		return 0
	case "roundRect", "triangle":
		return 1
	case "rightArrow", "leftArrow", "upArrow", "downArrow":
		return 2
	default:
		return -1
	}
}
