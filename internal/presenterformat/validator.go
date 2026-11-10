package presenterformat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
)

var ErrInvalidDeck = errors.New("invalid portable Presenter deck")

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

// ValidateV1 checks the legacy and v1 portable graph before a deck can become a cloud head.
// Embedded raster bytes retain the caller's existing media validation.
func ValidateV1(ctx context.Context, reader io.Reader, maxBytes int64, checkRaster func(context.Context, string, string, []byte) error) error {
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
	bounded := &io.LimitedReader{R: contextReader{ctx: ctx, reader: reader}, N: maxBytes + 1}
	decoder := json.NewDecoder(bounded)
	if decoder.Decode(&deck) != nil || decoder.Decode(&struct{}{}) != io.EOF || bounded.N == 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		return ErrInvalidDeck
	}
	if deck.SchemaVersion != nil && *deck.SchemaVersion != 1 {
		return ErrInvalidDeck
	}
	if strings.TrimSpace(deck.ID) == "" || strings.TrimSpace(deck.Name) == "" || !personalDimension(deck.Width) || !personalDimension(deck.Height) || deck.Slides == nil || len(deck.Slides) > 2000 || len(deck.Assets) > 10000 || len(deck.Themes) > 2000 || !personalOrder(deck.SlideOrder, deck.Slides) {
		return ErrInvalidDeck
	}
	if deck.DefaultThemeID != "" && deck.Themes[deck.DefaultThemeID] == nil {
		return ErrInvalidDeck
	}
	total := 0
	for id, slide := range deck.Slides {
		if id != slide.ID || (slide.ThemeID != "" && deck.Themes[slide.ThemeID] == nil) || !personalOrder(slide.ElementOrder, slide.Elements) {
			return ErrInvalidDeck
		}
		total += len(slide.Elements)
		if total > 100000 || len(slide.Elements) > 10000 {
			return ErrInvalidDeck
		}
		for id, e := range slide.Elements {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if e.ID != id || !personalCoordinate(e.X) || !personalCoordinate(e.Y) || !personalCoordinate(e.Rotation) || math.IsNaN(e.Opacity) || e.Opacity < 0 || e.Opacity > 1 {
				return ErrInvalidDeck
			}
			if e.Type == "line" {
				if !personalCoordinate(e.Width) || !personalCoordinate(e.Height) {
					return ErrInvalidDeck
				}
			} else if !personalDimension(e.Width) || !personalDimension(e.Height) {
				return ErrInvalidDeck
			}
			switch e.Type {
			case "image":
				if _, ok := deck.Assets[e.AssetID]; !ok {
					return ErrInvalidDeck
				}
			case "text", "shape", "line", "locked":
			default:
				return ErrInvalidDeck
			}
		}
	}
	for id, a := range deck.Assets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if id == "" || a.ID != id {
			return ErrInvalidDeck
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
			return ErrInvalidDeck
		}
		prefix := "data:" + a.MIMEType + ";base64,"
		if !strings.HasPrefix(a.DataURL, prefix) || len(a.DataURL)-len(prefix) > base64.StdEncoding.EncodedLen(20<<20) {
			return ErrInvalidDeck
		}
		data, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(a.DataURL, prefix))
		if err != nil || len(data) > 20<<20 {
			return ErrInvalidDeck
		}
		if err := checkRaster(ctx, "image"+extension, a.MIMEType, data); err != nil {
			return ErrInvalidDeck
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

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(value []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(value)
}
