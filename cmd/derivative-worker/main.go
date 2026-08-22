package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"hhc/asset-api/internal/derivativequeue"
	"hhc/asset-api/internal/derivatives"
	"hhc/asset-api/internal/postgres"
	azurestorage "hhc/asset-api/internal/storage/azure"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	if err := run(context.Background()); err != nil {
		slog.Error("asset derivative worker failed", "error", err)
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
	queueURL, err := required("ASSET_DERIVATIVE_QUEUE_URL")
	if err != nil {
		return err
	}
	poisonURL, err := required("ASSET_DERIVATIVE_POISON_QUEUE_URL")
	if err != nil {
		return err
	}
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
	queue, err := derivativequeue.NewAzureQueue(queueURL, poisonURL)
	if err != nil {
		return err
	}
	job := derivativequeue.NewJob(derivatives.NewWorker(postgres.New(db), blobs), queue, 5)
	processed, err := job.RunOnce(ctx)
	if err == nil {
		slog.Info("asset derivative worker finished", "processed", processed)
	}
	return err
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
