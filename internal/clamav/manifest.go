package clamav

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var signatureName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,120}$`)

var ErrStaleSignatures = errors.New("ClamAV signatures are stale")

type Manifest struct {
	Version                  int      `json:"version"`
	SignatureVersion         string   `json:"signatureVersion"`
	PreviousSignatureVersion string   `json:"previousSignatureVersion,omitempty"`
	LastSuccessfulAt         string   `json:"lastSuccessfulAt"`
	DatabaseDirectory        string   `json:"databaseDirectory"`
	Files                    []string `json:"files"`
}

func LoadManifest(path string, now time.Time, maxAge time.Duration) (Manifest, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, "", fmt.Errorf("read ClamAV manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, "", fmt.Errorf("decode ClamAV manifest: %w", err)
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, manifest.LastSuccessfulAt)
	if err != nil || manifest.Version != 1 || !signatureName.MatchString(manifest.SignatureVersion) || manifest.DatabaseDirectory != "sets/"+manifest.SignatureVersion || !validSignatureFiles(manifest.Files) {
		return Manifest{}, "", errors.New("invalid ClamAV manifest")
	}
	if updatedAt.After(now) || now.Sub(updatedAt) > maxAge {
		return Manifest{}, "", ErrStaleSignatures
	}
	database := filepath.Join(filepath.Dir(path), filepath.FromSlash(manifest.DatabaseDirectory))
	return manifest, database, nil
}

func validSignatureFiles(files []string) bool {
	if len(files) != 3 {
		return false
	}
	seen := map[string]bool{}
	for _, file := range files {
		base := strings.TrimSuffix(strings.TrimSuffix(file, ".cvd"), ".cld")
		if (base != "main" && base != "daily" && base != "bytecode") || (file != base+".cvd" && file != base+".cld") || seen[base] {
			return false
		}
		seen[base] = true
	}
	return len(seen) == 3
}
