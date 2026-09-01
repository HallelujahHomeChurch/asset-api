package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"hhc/asset-api/internal/clamav"
	"hhc/asset-api/internal/postgres"
	"hhc/asset-api/internal/scanqueue"
	azurestorage "hhc/asset-api/internal/storage/azure"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("asset scan worker failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	databaseURL, err := required("DATABASE_URL")
	if err != nil {
		return err
	}
	accountURL, err := required("ASSET_AZURE_ACCOUNT_URL")
	if err != nil {
		return err
	}
	queueURL, err := required("ASSET_SCAN_QUEUE_URL")
	if err != nil {
		return err
	}
	poisonURL, err := required("ASSET_SCAN_POISON_QUEUE_URL")
	if err != nil {
		return err
	}
	signatureContainer := value("CLAMAV_SIGNATURE_CONTAINER", "asset-signatures")
	timeout, err := positiveDuration("CLAMAV_SCAN_TIMEOUT", 10*time.Minute)
	if err != nil {
		return err
	}
	maxAge, err := positiveDuration("CLAMAV_SIGNATURE_MAX_AGE", 7*24*time.Hour)
	if err != nil {
		return err
	}
	maxSize, err := positiveInt64("ASSET_SCAN_MAX_FILE_SIZE_BYTES", 200<<20)
	if err != nil {
		return err
	}
	maxFileSize, err := positiveInt64("CLAMAV_MAX_FILE_SIZE_BYTES", 200<<20)
	if err != nil {
		return err
	}
	maxScanSize, err := positiveInt64("CLAMAV_MAX_SCAN_SIZE_BYTES", 1<<30)
	if err != nil {
		return err
	}
	maxFiles, err := positiveInt("CLAMAV_MAX_FILES", 10000)
	if err != nil {
		return err
	}
	maxRecursion, err := positiveInt("CLAMAV_MAX_RECURSION", 32)
	if err != nil {
		return err
	}
	maxAttempts, err := positiveInt("CLAMAV_MAX_RETRIES", 5)
	if err != nil {
		return err
	}
	idle, err := positiveDuration("ASSET_SCAN_IDLE_POLL", time.Second)
	if err != nil {
		return err
	}

	signatures, err := clamav.NewAzureSignatures(accountURL, signatureContainer)
	if err != nil {
		return err
	}
	manifest, signatureDirectory, cleanup, err := signatures.PrepareCurrent(ctx, time.Now().UTC(), maxAge)
	if err != nil {
		return err
	}
	defer cleanup()
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	blobs, err := azurestorage.New(accountURL, value("ASSET_AZURE_CONTAINER", "assets"))
	if err != nil {
		return err
	}
	queue, err := scanqueue.NewAzureQueue(queueURL, poisonURL)
	if err != nil {
		return err
	}
	scanner := clamav.NewLocalScanner(signatureDirectory, timeout, maxFileSize, maxScanSize, maxFiles, maxRecursion)
	job := scanqueue.NewScanJob(postgres.New(db), blobs, scanner, queue, manifest.SignatureVersion, maxSize, maxAttempts, timeout)
	return runLoop(ctx, job.RunOnce, idle)
}

type scanOnce func(context.Context) (bool, error)

func runLoop(ctx context.Context, run scanOnce, idle time.Duration) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		processed, err := run(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if processed {
			continue
		}
		timer := time.NewTimer(idle)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}

func required(key string) (string, error) {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value, nil
	}
	return "", fmt.Errorf("%s is required", key)
}
func value(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
func positiveInt(key string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return parsed, nil
}
func positiveInt64(key string, fallback int64) (int64, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return parsed, nil
}
func positiveDuration(key string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("invalid %s", key)
	}
	return parsed, nil
}
