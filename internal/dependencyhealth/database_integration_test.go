package dependencyhealth

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestDatabaseAcquireDeadlineAndRecovery(t *testing.T) {
	dsn := os.Getenv("DEPENDENCY_HEALTH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires explicit disposable loopback DB")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Host != "127.0.0.1" || !strings.HasSuffix(cfg.Database, "_test") {
		t.Fatal("requires disposable loopback test database")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	held, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	var b bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&b, nil))
	checkDB(context.Background(), db, logger)
	if !strings.Contains(b.String(), `"phase":"acquire"`) || !strings.Contains(b.String(), `"outcome":"deadline"`) {
		t.Fatal(b.String())
	}
	_ = held.Close()
	b.Reset()
	checkDB(context.Background(), db, logger)
	if !strings.Contains(b.String(), `"phase":"ping"`) || !strings.Contains(b.String(), `"outcome":"ok"`) {
		t.Fatal(b.String())
	}
	if db.Stats().InUse != 0 {
		t.Fatal("monitor leaked connection")
	}
}
