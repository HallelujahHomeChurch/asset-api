package docs_test

import (
	"gopkg.in/yaml.v3"
	"os"
	"testing"
)

func TestCaptureContractBounds(t *testing.T) {
	raw, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths      map[string]map[string]any `yaml:"paths"`
		Components struct {
			Schemas map[string]map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err = yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/priv/recording-captures", "/priv/recording-captures/{captureId}", "/priv/recording-captures/{captureId}/objects", "/priv/recording-captures/{captureId}/confirm", "/priv/recording-captures/{captureId}/sign", "/priv/recording-captures/{captureId}/seal", "/priv/recording-captures/{captureId}/abort", "/priv/recording-captures/{captureId}/progress", "/priv/recording-captures/{captureId}/grant"} {
		if doc.Paths[path] == nil {
			t.Fatalf("missing %s", path)
		}
	}
	for _, name := range []string{"RecordingCaptureCreateInput", "RecordingCaptureDeclareInput", "RecordingCaptureConfirmInput", "RecordingCaptureSealInput", "RecordingCaptureStatus", "RecordingLiveProgress"} {
		if doc.Components.Schemas[name] == nil {
			t.Fatalf("missing %s", name)
		}
	}
	props := doc.Components.Schemas["RecordingCaptureDeclareInput"]["properties"].(map[string]any)
	if props["objects"].(map[string]any)["maxItems"] != 100 {
		t.Fatal("declaration batch must be bounded to 100")
	}
}

func TestCaptureGrantDoesNotRequireUploaderActor(t *testing.T) {
	raw, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]yaml.Node `yaml:"paths"`
	}
	if err = yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var grant struct {
		Post struct {
			Parameters []map[string]any `yaml:"parameters"`
		} `yaml:"post"`
	}
	node := doc.Paths["/priv/recording-captures/{captureId}/grant"]
	if err = node.Decode(&grant); err != nil {
		t.Fatal(err)
	}
	for _, parameter := range grant.Post.Parameters {
		if parameter["$ref"] == "#/components/parameters/RecordingActorID" || parameter["name"] == "X-HHC-Actor-ID" {
			t.Fatal("live grant requires uploader identity instead of owner recording/scope authorization")
		}
	}
}
