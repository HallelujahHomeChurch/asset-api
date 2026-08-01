package clamav

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRefreshSignaturesPromotesValidatedSetAndKeepsPrevious(t *testing.T) {
	root := t.TempDir()
	previous := filepath.Join(root, "current", "sets", "clamav-old")
	if err := os.MkdirAll(previous, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "current", "manifest.json"), []byte(`{"version":1,"signatureVersion":"clamav-old","lastSuccessfulAt":"2026-07-01T00:00:00Z","databaseDirectory":"sets/clamav-old","files":["main.cvd","daily.cvd","bytecode.cvd"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := func(_ context.Context, command string, args ...string) error {
		if command == "freshclam" {
			var directory string
			for _, arg := range args {
				if strings.HasPrefix(arg, "--datadir=") {
					directory = strings.TrimPrefix(arg, "--datadir=")
				}
			}
			for _, name := range []string{"main.cvd", "daily.cvd", "bytecode.cvd"} {
				if err := os.WriteFile(filepath.Join(directory, name), []byte("db"), 0o600); err != nil {
					return err
				}
			}
		}
		return nil
	}
	now := time.Date(2026, 8, 1, 1, 2, 3, 0, time.UTC)
	manifest, err := RefreshSignatures(context.Background(), root, now, runner)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SignatureVersion == "" {
		t.Fatal("missing signature version")
	}
	if _, err := os.Stat(filepath.Join(root, "current", "sets", manifest.SignatureVersion, "daily.cvd")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(previous); err != nil {
		t.Fatal("previous known-good signature was removed")
	}
}
