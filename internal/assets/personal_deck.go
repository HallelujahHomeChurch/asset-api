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
	ID       string  `json:"id"`
	Type     string  `json:"type"`
	AssetID  string  `json:"assetId"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	Width    float64 `json:"width"`
	Height   float64 `json:"height"`
	Rotation float64 `json:"rotation"`
	Opacity  float64 `json:"opacity"`
}

// ValidatePersonalDeck checks the portable document graph before it can become a cloud head.
// Legacy documents without a schema version use the same graph; clients migrate them to v1.
func ValidatePersonalDeck(ctx context.Context, reader io.Reader) error {
	var deck struct {
		SchemaVersion *int     `json:"schemaVersion"`
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		Width         float64  `json:"width"`
		Height        float64  `json:"height"`
		SlideOrder    []string `json:"slideOrder"`
		Slides        map[string]struct {
			ID           string                         `json:"id"`
			ThemeID      string                         `json:"themeId"`
			ElementOrder []string                       `json:"elementOrder"`
			Elements     map[string]personalDeckElement `json:"elements"`
		} `json:"slides"`
		Assets map[string]struct {
			ID       string `json:"id"`
			MIMEType string `json:"mimeType"`
			DataURL  string `json:"dataUrl"`
		} `json:"assets"`
		Themes         map[string]json.RawMessage `json:"themes"`
		DefaultThemeID string                     `json:"defaultThemeId"`
	}
	decoder := json.NewDecoder(&contextReader{ctx: ctx, reader: io.LimitReader(reader, PersonalMaxFileSize+1)})
	if err := decoder.Decode(&deck); err != nil {
		return ErrInvalidUpload
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ErrInvalidUpload
	}
	if deck.SchemaVersion != nil && *deck.SchemaVersion != 1 {
		return ErrInvalidUpload
	}
	if strings.TrimSpace(deck.ID) == "" || strings.TrimSpace(deck.Name) == "" || !personalDimension(deck.Width) || !personalDimension(deck.Height) || deck.Slides == nil || len(deck.Slides) > 2000 || len(deck.Assets) > 10000 || len(deck.Themes) > 2000 || !personalOrder(deck.SlideOrder, deck.Slides) {
		return ErrInvalidUpload
	}
	if deck.DefaultThemeID != "" && deck.Themes[deck.DefaultThemeID] == nil {
		return ErrInvalidUpload
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
	return ctx.Err()
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
