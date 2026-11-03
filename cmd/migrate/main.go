package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"hhc/asset-api/internal/postgres"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"hhc/asset-api/internal/logging"
	"hhc/asset-api/internal/migrations"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	operation, err := parseOperation(os.Args[1:])
	if err != nil {
		slog.Error("invalid operation", "error", err)
		os.Exit(2)
	}
	if err := logging.Init(); err != nil {
		slog.Error("invalid logging configuration", "error", err)
		os.Exit(1)
	}
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		slog.Error("DATABASE_URL is required")
		os.Exit(1)
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if operation == "plan" || operation == "apply" {
		store := postgres.NewRecordingPackageStore(db)
		var enqueued int64
		if operation == "apply" {
			enqueued, err = store.EnqueueRecordingAudio(ctx)
			if err != nil {
				slog.Error("audio enqueue failed", "error", err)
				os.Exit(1)
			}
		}
		plan, err := store.PlanRecordingAudio(ctx)
		if err != nil {
			slog.Error("audio plan failed", "error", err)
			os.Exit(1)
		}
		var output any = plan
		if operation == "apply" {
			output = struct {
				Enqueued  int64                       `json:"enqueued"`
				Remaining postgres.RecordingAudioPlan `json:"remaining"`
			}{enqueued, plan}
		}
		if err := json.NewEncoder(os.Stdout).Encode(output); err != nil {
			os.Exit(1)
		}
		return
	}
	if err := migrations.Run(ctx, db); err != nil {
		slog.Error("apply migrations", "error", err)
		os.Exit(1)
	}
	slog.Info("migrations complete")
}

func parseOperation(args []string) (string, error) {
	flags := flag.NewFlagSet("asset-migrate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	plan := flags.Bool("recording-audio-plan", false, "read-only historical audio count/cost inputs; no migrations or enqueue")
	apply := flags.Bool("recording-audio-apply", false, "enqueue at most 100 eligible historical packages; requires approved plan; no migrations")
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if flags.NArg() != 0 || flags.NFlag() > 1 || len(args) > 0 && !*plan && !*apply {
		return "", fmt.Errorf("select only one audio operation and no positional arguments")
	}
	if *plan {
		return "plan", nil
	}
	if *apply {
		return "apply", nil
	}
	return "", nil
}
