package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestCanceledMigrationTransactionKeepsSessionCleanupOwned(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("HHW_TEST_DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("disposable loopback PostgreSQL required")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || !strings.Contains(u.Path, "test") {
		t.Fatal("loopback test database required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	key := fmt.Sprintf("migration-cancel-%d", time.Now().UnixNano())
	for i := 0; i < 200; i++ {
		if _, err = conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtext($1))`, key); err != nil {
			t.Fatal(err)
		}
		call, cancel := context.WithCancel(ctx)
		tx, err := beginMigrationTransaction(call, conn)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
		if err = tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if _, err = conn.ExecContext(ctx, `SELECT pg_advisory_unlock(hashtext($1))`, key); err != nil {
			t.Fatal(err)
		}
	}
	// An aborted transaction must never report a successful COMMIT.
	tx, err := beginMigrationTransaction(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, "SELECT 1/0"); err == nil {
		t.Fatal("synthetic SQL error absent")
	}
	if err = tx.Commit(); err == nil {
		t.Fatal("aborted transaction reported a successful commit")
	}
	if err = conn.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	var locks int
	if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory'`).Scan(&locks); err != nil || locks != 0 {
		t.Fatalf("session retains%dlocks: %v", locks, err)
	}
}
