package clamav

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAzureSignaturesRoundTripImmutableGeneration(t *testing.T) {
	blobs := map[string][]byte{}
	store := &AzureSignatures{
		download: func(_ context.Context, name string) ([]byte, error) {
			value, ok := blobs[name]
			if !ok {
				return nil, os.ErrNotExist
			}
			return value, nil
		},
		upload: func(_ context.Context, name, path string, immutable bool) error {
			if immutable {
				if _, exists := blobs[name]; exists {
					t.Fatalf("immutable blob overwritten: %s", name)
				}
			}
			data, err := os.ReadFile(path)
			blobs[name] = data
			return err
		},
	}
	root := t.TempDir()
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	manifest := Manifest{Version: 1, SignatureVersion: "clamav-new", LastSuccessfulAt: now.Format(time.RFC3339Nano), DatabaseDirectory: "sets/clamav-new", Files: []string{"main.cvd", "daily.cvd", "bytecode.cvd"}}
	directory := filepath.Join(root, "current", "sets", "clamav-new")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range manifest.Files {
		if err := os.WriteFile(filepath.Join(directory, file), []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Publish(context.Background(), root, manifest); err != nil {
		t.Fatal(err)
	}
	prepared, database, cleanup, err := store.PrepareCurrent(context.Background(), now.Add(time.Hour), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if prepared.SignatureVersion != manifest.SignatureVersion {
		t.Fatalf("manifest=%+v", prepared)
	}
	for _, file := range manifest.Files {
		if _, err := os.Stat(filepath.Join(database, file)); err != nil {
			t.Fatal(err)
		}
	}
	var current Manifest
	if err := json.Unmarshal(blobs["current.json"], &current); err != nil {
		t.Fatal(err)
	}
}
