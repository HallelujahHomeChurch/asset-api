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
	"hhc/asset-api/internal/logging"
	"hhc/asset-api/internal/telemetry"
)

func main() {
	if err := logging.Init(); err != nil {
		slog.Error("invalid logging configuration", "error", err)
		os.Exit(1)
	}
	if err := run(context.Background()); err != nil {
		slog.Error("signature job failed")
		os.Exit(1)
	}
}

func run(ctx context.Context) (result error) {
	ctx, finishTelemetry := telemetry.StartWorker(ctx, "signature")
	defer func() { finishTelemetry(result) }()
	accountURL := strings.TrimSpace(os.Getenv("ASSET_AZURE_ACCOUNT_URL"))
	if accountURL == "" {
		return fmt.Errorf("ASSET_AZURE_ACCOUNT_URL is required")
	}
	container := strings.TrimSpace(os.Getenv("CLAMAV_SIGNATURE_CONTAINER"))
	if container == "" {
		container = "asset-signatures"
	}
	root, err := os.MkdirTemp("", "clamav-refresh-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	timeout := 15 * time.Minute
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	manifest, err := clamav.RefreshSignatures(ctx, root, time.Now().UTC(), func(ctx context.Context, command string, args ...string) error {
		output, err := exec.CommandContext(ctx, command, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	store, err := clamav.NewAzureSignatures(accountURL, container)
	if err == nil {
		manifest, err = store.Publish(ctx, root, manifest)
	}
	if err != nil {
		return err
	}
	slog.Info("ClamAV signatures refreshed", "signatureVersion", manifest.SignatureVersion)
	return nil
}
