package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"hhc/asset-api/internal/config"
	"hhc/asset-api/internal/logging"
	"hhc/asset-api/internal/postgres"
	"hhc/asset-api/internal/recordingprocessing"
	"hhc/asset-api/internal/recordingvalidation"
	azurestorage "hhc/asset-api/internal/storage/azure"
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
	if !hlsEnabled {
		return nil
	}
	sourceAccount, sourceContainer, err := config.RecordingSourceStorage(hlsEnabled)
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
		cleanupErr := packages.ReconcilePackages(ctx, objects.DeletePackageObjects)
		if sourceAccount != "" {
			sources, err := azurestorage.New(sourceAccount, sourceContainer)
			if err != nil {
				return errors.Join(cleanupErr, err)
			}
			repository := postgres.NewRecordingSourceStore(db)
			cleanupErr = errors.Join(cleanupErr, repository.ReconcileSources(ctx, sources.DeleteRecordingSource, objects.DeleteSourceAttempt))
			// One long processing claim per execution keeps the 5.5-hour app
			// deadline inside the six-hour platform Job timeout.
			processed, err := recordingprocessing.RunSourceProcessing(ctx, repository, sources, objects, probe)
			if processed || err != nil {
				return errors.Join(cleanupErr, err)
			}
		}
		validationErr := recordingvalidation.RunPackageValidation(ctx, packages, objects, probe)
		return errors.Join(validationErr, cleanupErr)
	}
	return nil
}
