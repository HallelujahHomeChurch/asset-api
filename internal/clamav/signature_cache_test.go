package clamav

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSignatureCacheChecksAgeAndSwitchesAcrossDays(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	current := cacheManifest("first", now)
	reads := map[string]int{}
	store := &AzureSignatures{download: func(_ context.Context, name string) ([]byte, error) {
		reads[name]++
		if name == "current.json" {
			return json.Marshal(current)
		}
		return []byte(name), nil
	}}
	cache := NewSignatureCache(store)
	defer cache.Close()
	_, first, err := cache.Prepare(context.Background(), now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, directory, err := cache.Prepare(context.Background(), now.Add(time.Hour), 24*time.Hour); err != nil || directory != first {
		t.Fatalf("unchanged generation: directory=%q err=%v", directory, err)
	}
	if reads["current.json"] != 2 || reads["sets/first/main.cvd"] != 1 {
		t.Fatalf("cache did not recheck manifest or redownloaded files: %v", reads)
	}
	if _, directory, err := cache.Prepare(context.Background(), now.Add(25*time.Hour), 24*time.Hour); !errors.Is(err, ErrStaleSignatures) || directory != "" {
		t.Fatalf("cached generation bypassed expiry: %q %v", directory, err)
	}
	current = cacheManifest("second", now.Add(24*time.Hour))
	manifest, second, err := cache.Prepare(context.Background(), now.Add(25*time.Hour), 24*time.Hour)
	if err != nil || manifest.SignatureVersion != "second" || second == first {
		t.Fatalf("cross-day switch: %+v %q %v", manifest, second, err)
	}
	if _, err := os.Stat(first); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old generation retained: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(second, "daily.cvd"))
	if err != nil || string(data) != "sets/second/daily.cvd" {
		t.Fatalf("incomplete new generation: %q %v", data, err)
	}
}

func TestSignatureCacheFailedPreparationNeverReplacesUsableGeneration(t *testing.T) {
	for _, failure := range []error{errors.New("download failed"), context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("TMPDIR", root)
			now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
			current := cacheManifest("first", now)
			fail := false
			store := &AzureSignatures{download: func(_ context.Context, name string) ([]byte, error) {
				if name == "current.json" {
					return json.Marshal(current)
				}
				if fail && name == "sets/second/daily.cvd" {
					return nil, failure
				}
				return []byte(name), nil
			}}
			cache := NewSignatureCache(store)
			defer cache.Close()
			_, original, err := cache.Prepare(context.Background(), now, 24*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			current, fail = cacheManifest("second", now), true
			if manifest, directory, err := cache.Prepare(context.Background(), now, 24*time.Hour); !errors.Is(err, failure) || directory != "" || manifest.SignatureVersion != "" {
				t.Fatalf("failed preparation allowed scanning: %+v %q %v", manifest, directory, err)
			}
			if cache.directory != original || cache.manifest.SignatureVersion != "first" {
				t.Fatal("partial generation replaced cache")
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 {
				t.Fatalf("partial download leaked: entries=%d err=%v", len(entries), err)
			}
			fail = false
			if manifest, _, err := cache.Prepare(context.Background(), now, 24*time.Hour); err != nil || manifest.SignatureVersion != "second" {
				t.Fatalf("could not recover: %+v %v", manifest, err)
			}
		})
	}
}

func TestSignatureCacheRejectsManifestDriftAndReadFailure(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	current := cacheManifest("first", now)
	unavailable := false
	store := &AzureSignatures{download: func(_ context.Context, name string) ([]byte, error) {
		if unavailable {
			return nil, os.ErrNotExist
		}
		if name == "current.json" {
			return json.Marshal(current)
		}
		return []byte(name), nil
	}}
	cache := NewSignatureCache(store)
	defer cache.Close()
	if _, _, err := cache.Prepare(context.Background(), now, time.Hour); err != nil {
		t.Fatal(err)
	}
	current.PreviousSignatureVersion = "changed"
	if _, directory, err := cache.Prepare(context.Background(), now, time.Hour); err == nil || directory != "" {
		t.Fatal("immutable manifest drift accepted")
	}
	unavailable = true
	if _, directory, err := cache.Prepare(context.Background(), now, time.Hour); !errors.Is(err, os.ErrNotExist) || directory != "" {
		t.Fatal("cached signatures used without checking current manifest")
	}
}

func cacheManifest(version string, now time.Time) Manifest {
	return Manifest{Version: 1, SignatureVersion: version, LastSuccessfulAt: now.Format(time.RFC3339Nano), DatabaseDirectory: "sets/" + version, Files: []string{"main.cvd", "daily.cvd", "bytecode.cvd"}}
}
