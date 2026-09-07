package assets

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestPersonalDeckValidation(t *testing.T) {
	valid := `{"schemaVersion":1,"id":"deck","name":"Deck","width":1920,"height":1080,"slideOrder":["s"],"slides":{"s":{"id":"s","elementOrder":["e"],"elements":{"e":{"id":"e","type":"image","assetId":"image","x":0,"y":0,"width":100,"height":100,"rotation":0,"opacity":1}}}},"assets":{"image":{"id":"image","mimeType":"image/png","dataUrl":"data:image/png;base64,iVBORw0KGgo="}}}`
	for _, test := range []struct {
		name, body string
		ok         bool
	}{
		{"valid", valid, true}, {"legacy", strings.Replace(valid, `"schemaVersion":1,`, "", 1), true},
		{"future", strings.Replace(valid, `"schemaVersion":1`, `"schemaVersion":2`, 1), false},
		{"reference", strings.Replace(valid, `"assetId":"image"`, `"assetId":"missing"`, 1), false},
		{"duplicate-order", strings.Replace(valid, `["e"]`, `["e","e"]`, 1), false},
		{"zero-size", strings.Replace(valid, `"width":1920`, `"width":0`, 1), false},
		{"external", strings.Replace(valid, "data:image/png;base64,iVBORw0KGgo=", "https://example.test/image.png", 1), false},
		{"svg", strings.ReplaceAll(strings.Replace(valid, "iVBORw0KGgo=", "PHN2Zz48c2NyaXB0Lz48L3N2Zz4=", 1), "image/png", "image/svg+xml"), false},
		{"trailing", valid + `{}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidatePersonalDeck(context.Background(), strings.NewReader(test.body))
			if (err == nil) != test.ok {
				t.Fatalf("err=%v", err)
			}
		})
	}
	var deck map[string]any
	if err := json.Unmarshal([]byte(valid), &deck); err != nil {
		t.Fatal(err)
	}
	deck["width"] = 1e100
	payload, _ := json.Marshal(deck)
	if err := ValidatePersonalDeck(context.Background(), bytes.NewReader(payload)); err == nil {
		t.Fatal("unbounded dimensions")
	}
}
