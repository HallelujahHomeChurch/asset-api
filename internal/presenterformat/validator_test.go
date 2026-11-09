package presenterformat

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
)

// Fixtures use the Presenter v1 writer and validator at 8f6c0a2edebe9bc33a9f38addda68c94e4cc9658.
func TestPortableV1Contract(t *testing.T) {
	valid, err := os.ReadFile("testdata/portable-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	invalid, err := os.ReadFile("testdata/portable-v1-missing-image.json")
	if err != nil {
		t.Fatal(err)
	}
	legacy := bytes.Replace(valid, []byte("  \"schemaVersion\": 1,\n"), nil, 1)
	if bytes.Equal(valid, legacy) {
		t.Fatal("fixture version was not removed")
	}
	checkImage := func(_ context.Context, name, mime string, data []byte) error {
		if name != "image.png" || mime != "image/png" || !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
			t.Fatalf("invalid embedded raster arguments: name=%s mime=%s", name, mime)
		}
		return nil
	}
	for _, test := range []struct {
		name string
		body []byte
		max  int64
		want error
	}{
		{"v1 exact limit", valid, int64(len(valid)), nil},
		{"legacy", legacy, int64(len(legacy)), nil},
		{"broken image reference", invalid, int64(len(invalid)), ErrInvalidDeck},
		{"trailing bytes beyond limit", append(bytes.Clone(valid), ' '), int64(len(valid)), ErrInvalidDeck},
		{"truncated", valid[:len(valid)-10], int64(len(valid)), ErrInvalidDeck},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateV1(context.Background(), bytes.NewReader(test.body), test.max, checkImage)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ValidateV1(ctx, bytes.NewReader(valid), int64(len(valid)), checkImage); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}
