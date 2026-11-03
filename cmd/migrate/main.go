package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"hhc/asset-api/internal/postgres"
	"log/slog"
	"os"
	"strings"
	"time"

	"hhc/asset-api/internal/logging"
	"hhc/asset-api/internal/migrations"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	audioPlan := flag.Bool("recording-audio-plan", false, "read-only historical audio count/cost inputs; no migrations or enqueue")
	flag.Parse()
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
	if *audioPlan {
		plan, err := postgres.NewRecordingPackageStore(db).PlanRecordingAudio(ctx)
		if err != nil {
			slog.Error("audio plan failed", "error", err)
			os.Exit(1)
		}
		if err := json.NewEncoder(os.Stdout).Encode(plan); err != nil {
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
