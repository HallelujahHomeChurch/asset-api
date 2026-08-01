package clamav

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadManifestRejectsStaleOrEscapingSignatureSets(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "manifest.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"version":1,"signatureVersion":"clamav-1","lastSuccessfulAt":"2026-07-31T00:00:00Z","databaseDirectory":"sets/clamav-1","files":["main.cvd","daily.cvd","bytecode.cvd"]}`)
	manifest, directory, err := LoadManifest(path, now, 48*time.Hour)
	if err != nil || manifest.SignatureVersion != "clamav-1" || directory != filepath.Join(filepath.Dir(path), "sets", "clamav-1") {
		t.Fatalf("valid manifest = %+v %q %v", manifest, directory, err)
	}
	write(`{"version":1,"signatureVersion":"clamav-1","lastSuccessfulAt":"2026-07-20T00:00:00Z","databaseDirectory":"sets/clamav-1","files":["main.cvd","daily.cvd","bytecode.cvd"]}`)
	if _, _, err := LoadManifest(path, now, 48*time.Hour); !errors.Is(err, ErrStaleSignatures) {
		t.Fatalf("stale error = %v", err)
	}
	write(`{"version":1,"signatureVersion":"clamav-1","lastSuccessfulAt":"2026-07-31T00:00:00Z","databaseDirectory":"../../etc","files":["main.cvd","daily.cvd","bytecode.cvd"]}`)
	if _, _, err := LoadManifest(path, now, 48*time.Hour); err == nil {
		t.Fatal("accepted escaping path")
	}
}
