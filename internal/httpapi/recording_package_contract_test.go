package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"hhc/asset-api/internal/assets"
)

func TestRecordingPackageWireFieldsMatchOpenAPI(t *testing.T) {
	data, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var api struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any
				Required   []string
			}
		}
	}
	if err := yaml.Unmarshal(data, &api); err != nil {
		t.Fatal(err)
	}
	count := api.Components.Schemas["RecordingRendition"].Properties["segmentCount"].(map[string]any)
	if count["minimum"] != 1 || count["maximum"] != 1440 || !strings.Contains(count["description"].(string), "Actual number of contiguous media segments") || strings.Contains(count["description"].(string), "ceil(") {
		t.Fatalf("segment count contract must use actual bounded media: %+v", count)
	}
	for name, value := range map[string]any{
		"BroadcastRangeInput":        assets.RecordingBroadcastRange{},
		"BroadcastProjection":        assets.RecordingBroadcastProjection{},
		"RecordingCover":             coverItem{OperationKey: "cover-upload"},
		"RecordingSource":            assets.RecordingSource{},
		"RecordingSourceStatus":      assets.RecordingSourceStatus{},
		"SignedRecordingSourceBlock": assets.SignedRecordingSourceBlock{},
		"RecordingPackage":           assets.RecordingPackage{},
		"RecordingPackageStatus":     assets.RecordingPackageStatus{},
		"RecordingPackageInventory":  assets.RecordingPackageInventory{InventoryDigest: strings.Repeat("a", 64)},
		"RecordingPackageObject":     assets.RecordingPackageObject{},
		"RecordingRendition":         assets.RecordingRendition{},
	} {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			schema, ok := api.Components.Schemas[name]
			if !ok {
				t.Fatalf("schema missing: %s", name)
			}
			for field := range fields {
				if _, ok := schema.Properties[field]; !ok {
					t.Errorf("undocumented wire field: %s", field)
				}
			}
			for _, field := range schema.Required {
				if _, ok := fields[field]; !ok {
					t.Errorf("missing required wire field: %s", field)
				}
			}
		})
	}
}

func TestRecordingSizeErrorsHaveStableHTTPCodes(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{assets.ErrRecordingSourceTooLarge, 413, "source_too_large"},
		{assets.ErrRecordingPackageTooLarge, 413, "package_too_large"},
		{assets.ErrRecordingPackageEstimateTooLarge, 422, "package_size_estimate_exceeded"},
	} {
		w := httptest.NewRecorder()
		handleError(w, tc.err)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
			t.Fatalf("error response: %d %s", w.Code, w.Body.String())
		}
	}
}
