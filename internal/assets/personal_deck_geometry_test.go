package assets

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
)

func TestPersonalDeckGeometryCorpus(t *testing.T) {
	raw, err := os.ReadFile("testdata/preset-geometry/contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Name     string          `json:"name"`
		Valid    bool            `json:"valid"`
		Document json.RawMessage `json:"document"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Run(entry.Name, func(t *testing.T) {
			before := bytes.Clone(entry.Document)
			err := ValidatePersonalDeck(context.Background(), bytes.NewReader(entry.Document))
			if (err == nil) != entry.Valid {
				t.Fatalf("valid=%v err=%v", entry.Valid, err)
			}
			if !bytes.Equal(before, entry.Document) {
				t.Fatal("validator rewrote source")
			}
		})
	}
}
