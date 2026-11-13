package assets

import (
	"encoding/json"
	"os"
	"testing"
)

func TestHarnessPublicationNamesMatchOwnerValidation(t *testing.T) {
	data, err := os.ReadFile("testdata/line-publication-names.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Input  string `json:"input"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 {
		t.Fatal("missing producer fixtures")
	}
	for _, row := range rows {
		if !validCollectionItemDisplayName(row.Output) {
			t.Fatalf("invalid producer output %q", row.Output)
		}
	}
}
