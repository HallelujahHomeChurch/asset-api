package clamav

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type CommandRunner func(context.Context, string, ...string) error

func RefreshSignatures(ctx context.Context, root string, now time.Time, run CommandRunner) (Manifest, error) {
	if !filepath.IsAbs(root) {
		return Manifest{}, errors.New("signature root must be absolute")
	}
	staging, err := os.MkdirTemp(root, ".staging-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(staging)
	if err := run(ctx, "freshclam", "--stdout", "--no-warnings", "--log=/dev/null", "--datadir="+staging); err != nil {
		return Manifest{}, fmt.Errorf("refresh ClamAV signatures: %w", err)
	}
	files := make([]string, 0, 3)
	for _, base := range []string{"main", "daily", "bytecode"} {
		file, err := oneDatabaseFile(staging, base)
		if err != nil {
			return Manifest{}, err
		}
		files = append(files, filepath.Base(file))
		if err := run(ctx, "sigtool", "--info", file); err != nil {
			return Manifest{}, fmt.Errorf("validate %s signatures: %w", base, err)
		}
	}
	version := "clamav-" + strings.NewReplacer("-", "", ":", "", ".", "").Replace(now.UTC().Format("20060102T150405.000000000Z"))
	manifest := Manifest{Version: 1, SignatureVersion: version, LastSuccessfulAt: now.UTC().Format(time.RFC3339Nano), DatabaseDirectory: "sets/" + version, Files: files}
	current := filepath.Join(root, "current")
	sets := filepath.Join(current, "sets")
	if err := os.MkdirAll(sets, 0o700); err != nil {
		return Manifest{}, err
	}
	previous := previousSignature(filepath.Join(current, "manifest.json"))
	promoted := filepath.Join(sets, version)
	if err := os.Rename(staging, promoted); err != nil {
		return Manifest{}, err
	}
	data, _ := json.Marshal(manifest)
	temporary := filepath.Join(current, ".manifest-"+version)
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		os.RemoveAll(promoted)
		return Manifest{}, err
	}
	if err := replaceManifest(temporary, filepath.Join(current, "manifest.json")); err != nil {
		os.RemoveAll(promoted)
		os.Remove(temporary)
		return Manifest{}, err
	}
	entries, _ := os.ReadDir(sets)
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != version && entry.Name() != previous && signatureName.MatchString(entry.Name()) {
			_ = os.RemoveAll(filepath.Join(sets, entry.Name()))
		}
	}
	return manifest, nil
}

func oneDatabaseFile(directory, base string) (string, error) {
	var found string
	for _, extension := range []string{".cvd", ".cld"} {
		path := filepath.Join(directory, base+extension)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || found != "" {
			return "", fmt.Errorf("invalid %s signature set", base)
		}
		found = path
	}
	if found == "" {
		return "", fmt.Errorf("missing %s signature set", base)
	}
	return found, nil
}

func previousSignature(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var manifest Manifest
	if json.Unmarshal(data, &manifest) != nil || !signatureName.MatchString(manifest.SignatureVersion) {
		return ""
	}
	return manifest.SignatureVersion
}

func replaceManifest(temporary, target string) error {
	if err := os.Rename(temporary, target); err == nil {
		return nil
	}
	previous, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	if err := os.Remove(target); err != nil {
		return err
	}
	if err := os.Rename(temporary, target); err != nil {
		_ = os.WriteFile(target, previous, 0o600)
		return err
	}
	return nil
}
