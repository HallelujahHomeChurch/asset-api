package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"hhc/asset-api/internal/postgres"
	"hhc/asset-api/internal/retention"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	if err := run(context.Background()); err != nil {
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return errorsLog("DATABASE_URL is required", nil)
	}
	apply, err := applyEnabled(os.Getenv("ASSET_RETENTION_APPLY_ENABLED"))
	if err != nil {
		return errorsLog("invalid retention configuration", err)
	}
	runID, err := newRunID()
	if err != nil {
		return errorsLog("create run ID", err)
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return errorsLog("open database", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)

	startedAt := time.Now().UTC()
	result, sweepErr := retention.NewWorker(postgres.New(db), apply).SweepExpiredCollectionItems(ctx, startedAt, retention.BatchSize)
	completedAt := time.Now().UTC()
	for _, preview := range result.Preview {
		slog.Info("asset retention preview", "runId", runID, "collectionId", preview.CollectionID, "candidateCount", preview.CandidateCount, "totalBytes", preview.TotalBytes)
	}
	fields := []any{
		"runId", runID, "applyEnabled", apply,
		"startedAt", startedAt, "completedAt", completedAt, "durationMs", completedAt.Sub(startedAt).Milliseconds(),
		"scanned", result.Scanned, "deleted", result.Deleted, "exemptSkipped", result.ExemptSkipped,
		"alreadyRemoved", result.AlreadyRemoved, "failedItems", result.FailedItems,
		"failedBatches", result.FailedBatches, "remainingBacklog", result.RemainingBacklog,
	}
	if sweepErr != nil {
		slog.Error("asset retention run finished", append(fields, "error", sweepErr)...)
	} else {
		slog.Info("asset retention run finished", fields...)
	}
	return sweepErr
}

func applyEnabled(value string) (bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return false, nil
	}
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("invalid ASSET_RETENTION_APPLY_ENABLED")
	}
	return enabled, nil
}

func newRunID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func errorsLog(message string, err error) error {
	if err == nil {
		err = errors.New(message)
	}
	slog.Error(message, "error", err)
	return err
}
