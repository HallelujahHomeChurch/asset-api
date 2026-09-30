package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/config"
	"hhc/asset-api/internal/logging"
	"hhc/asset-api/internal/postgres"
	"hhc/asset-api/internal/recordingvalidation"
	"hhc/asset-api/internal/storage/r2"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	if err := logging.Init(); err != nil {
		slog.Error("invalid logging configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("recording validation worker stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	hlsEnabled, err := config.RecordingHLSFlag()
	if err != nil {
		return err
	}
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	objects, err := r2.New(os.Getenv("ASSET_R2_ACCOUNT_ID"), os.Getenv("ASSET_R2_BUCKET"), os.Getenv("ASSET_R2_ACCESS_KEY_ID"), os.Getenv("ASSET_R2_SECRET_ACCESS_KEY"))
	if err != nil {
		return fmt.Errorf("recording storage configuration: %w", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(3)
	if hlsEnabled {
		packages := postgres.NewRecordingPackageStore(db)
		probe := recordingvalidation.PackageMediaProbe{Objects: objects, FFmpeg: "/usr/bin/ffmpeg", FFprobe: "/usr/bin/ffprobe"}
		validationErr := recordingvalidation.RunPackageValidation(ctx, packages, objects, probe)
		cleanupErr := packages.ReconcilePackages(ctx, objects.DeletePackageObjects)
		return errors.Join(validationErr, cleanupErr)
	}
	repository := postgres.NewRecordingUploadStore(db)
	worker := recordingvalidation.New(repository, objects.Open, func(ctx context.Context, key string) ([]byte, error) {
		url, err := objects.PresignProbeRead(ctx, key, 15*time.Minute)
		if err != nil {
			return nil, err
		}
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		// Never log the signed URL or ffprobe stderr.
		output, err := exec.CommandContext(probeCtx, "ffprobe", "-v", "error", "-show_entries", "stream=codec_type,codec_name:format=duration", "-of", "json", url.URL).Output()
		if err != nil {
			return nil, errors.New("ffprobe failed")
		}
		if len(output) > 1<<20 {
			return nil, errors.New("ffprobe output too large")
		}
		return output, nil
	}, time.Now)
	uploads := assets.NewRecordingUploadService(repository, objects, time.Now)
	return runOnce(ctx, worker.RunOnce, repository.ExpiredAbandoned, uploads.DeleteAbandoned, time.Now)
}

func runOnce(
	ctx context.Context,
	validate func(context.Context) error,
	expired func(context.Context, time.Time) ([]string, error),
	deleteAbandoned func(context.Context, string, time.Time) error,
	now func() time.Time,
) error {
	var errs []error
	if err := validate(ctx); err != nil {
		errs = append(errs, fmt.Errorf("recording validation: %w", err))
	}
	versions, err := expired(ctx, now())
	if err != nil {
		errs = append(errs, fmt.Errorf("recording cleanup query: %w", err))
	} else {
		for _, versionID := range versions {
			if err := deleteAbandoned(ctx, versionID, now()); err != nil {
				errs = append(errs, fmt.Errorf("recording cleanup %s: %w", versionID, err))
			}
		}
	}
	return errors.Join(errs...)
}
