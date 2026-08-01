package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"hhc/asset-api/internal/clamav"
)

func main() {
	accountURL := strings.TrimSpace(os.Getenv("ASSET_AZURE_ACCOUNT_URL"))
	if accountURL == "" {
		slog.Error("ASSET_AZURE_ACCOUNT_URL is required")
		os.Exit(1)
	}
	container := strings.TrimSpace(os.Getenv("CLAMAV_SIGNATURE_CONTAINER"))
	if container == "" {
		container = "asset-signatures"
	}
	root, err := os.MkdirTemp("", "clamav-refresh-")
	if err != nil {
		slog.Error("create refresh directory", "error", err)
		os.Exit(1)
	}
	defer os.RemoveAll(root)
	timeout := 15 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	manifest, err := clamav.RefreshSignatures(ctx, root, time.Now().UTC(), func(ctx context.Context, command string, args ...string) error {
		output, err := exec.CommandContext(ctx, command, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
		}
		return nil
	})
	if err != nil {
		slog.Error("ClamAV signature refresh failed", "error", err)
		os.Exit(1)
	}
	store, err := clamav.NewAzureSignatures(accountURL, container)
	if err == nil {
		manifest, err = store.Publish(ctx, root, manifest)
	}
	if err != nil {
		slog.Error("publish ClamAV signatures failed", "error", err)
		os.Exit(1)
	}
	slog.Info("ClamAV signatures refreshed", "signatureVersion", manifest.SignatureVersion)
}
