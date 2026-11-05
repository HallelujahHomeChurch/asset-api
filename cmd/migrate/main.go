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
	recovery, recovering, err := parseAudioRecovery(os.Args[1:])
	var operation string
	var limit int
	if err == nil && !recovering {
		operation, limit, err = parseOperation(os.Args[1:])
	}
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
	if recovering {
		store := postgres.NewRecordingPackageStore(db)
		var output any
		if recovery.apply {
			err = store.RetryRecordingAudio(ctx, recovery.packageID, recovery.lastClaim)
			output = struct {
				Enqueued           int `json:"enqueued"`
				AdditionalAttempts int `json:"additionalAttempts"`
			}{1, 1}
		} else {
			output, err = store.PlanRecordingAudioRetry(ctx, recovery.packageID)
		}
		if err != nil {
			slog.Error("audio recovery failed", "error", err)
			os.Exit(1)
		}
		if err = json.NewEncoder(os.Stdout).Encode(output); err != nil {
			os.Exit(1)
		}
		return
	}
	if operation == "plan" || operation == "apply" {
		store := postgres.NewRecordingPackageStore(db)
		var enqueued int64
		if operation == "apply" {
			enqueued, err = store.EnqueueRecordingAudio(ctx, limit)
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

func parseOperation(args []string) (string, int, error) {
	flags := flag.NewFlagSet("asset-migrate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	plan := flags.Bool("recording-audio-plan", false, "read-only historical audio count/cost inputs; no migrations or enqueue")
	apply := flags.Bool("recording-audio-apply", false, "enqueue at most 100 eligible historical packages; requires approved plan; no migrations")
	limit := flags.Int("recording-audio-limit", 100, "maximum packages in this approved apply batch (1..100); apply only")
	if err := flags.Parse(args); err != nil {
		return "", 0, err
	}
	var hasPlan, hasApply, hasLimit bool
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "recording-audio-plan":
			hasPlan = true
		case "recording-audio-apply":
			hasApply = true
		case "recording-audio-limit":
			hasLimit = true
		}
	})
	if flags.NArg() != 0 || hasPlan && hasApply || len(args) > 0 && !*plan && !*apply || hasLimit && !*apply || *limit < 1 || *limit > 100 {
		return "", 0, fmt.Errorf("select one audio operation with no positional arguments; audio limit requires apply and must be 1..100")
	}
	if *plan {
		return "plan", *limit, nil
	}
	if *apply {
		return "apply", *limit, nil
	}
	return "", *limit, nil
}
