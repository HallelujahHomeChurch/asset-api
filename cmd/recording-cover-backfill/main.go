package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	_ "github.com/jackc/pgx/v5/stdlib"
	"hhc/asset-api/internal/postgres"
	"os"
	"time"
)

func main() {
	apply := flag.Bool("apply", false, "enqueue the reviewed page (default is dry-run)")
	after := flag.String("after", "", "exclusive package cursor from the previous page")
	limit := flag.Int("limit", 20, "page size, at most 100")
	flag.Parse()
	if err := run(*after, *limit, *apply); err != nil {
		fmt.Fprintln(os.Stderr, "cover backfill failed; no success receipt")
		os.Exit(1)
	}
}
func run(after string, limit int, apply bool) error {
	if os.Getenv("DATABASE_URL") == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ids, err := postgres.NewRecordingCoverStore(db).Backfill(ctx, after, limit, apply)
	if err != nil {
		return err
	}
	next := after
	if len(ids) > 0 {
		next = ids[len(ids)-1]
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Apply      bool     `json:"apply"`
		PackageIDs []string `json:"packageIds"`
		Next       string   `json:"nextAfter"`
	}{apply, ids, next})
}
