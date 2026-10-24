package migrations

import (
	"context"
	"database/sql"
	"fmt"
	_ "github.com/jackc/pgx/v5/stdlib"
	"io/fs"
	"net/url"
	"os"
	"testing"
	"time"
)

func TestMigrationLockFailureDiscardsSession(t *testing.T) {
	for _, fault := range []string{"acquire", "unlock", "unlock-stall"} {
		t.Run(fault, func(t *testing.T) {
			dsn := os.Getenv("TEST_DATABASE_URL")
			if dsn == "" {
				t.Skip("disposable PostgreSQL required")
			}
			parsed, err := url.Parse(dsn)
			if err != nil || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost") {
				t.Fatal("loopback test database required")
			}
			admin, err := sql.Open("pgx", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer admin.Close()
			schema := fmt.Sprintf("migration_fault_%d", time.Now().UnixNano())
			if _, err = admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
				t.Fatal(err)
			}
			defer admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
			body := `BEGIN RAISE EXCEPTION 'synthetic unlock failure'; END`
			fn := "pg_advisory_unlock"
			if fault == "acquire" {
				fn = "pg_advisory_lock"
				body = `BEGIN PERFORM pg_catalog.pg_advisory_lock($1::bigint); RAISE EXCEPTION 'synthetic acquisition failure'; END`
			}
			if fault == "unlock-stall" {
				body = `BEGIN PERFORM pg_catalog.pg_sleep(30); RETURN pg_catalog.pg_advisory_unlock($1::bigint); END`
			}
			if _, err = admin.Exec(`CREATE FUNCTION ` + schema + `.` + fn + `(integer) RETURNS boolean LANGUAGE plpgsql AS $$ ` + body + ` $$`); err != nil {
				t.Fatal(err)
			}
			if fault != "acquire" {
				entries, err := fs.Glob(files, "sql/*.sql")
				if err != nil || len(entries) == 0 {
					t.Fatalf("embedded migrations: %v", err)
				}
				if _, err = admin.Exec(`CREATE TABLE ` + schema + `.schema_migrations(version text PRIMARY KEY, checksum text NOT NULL DEFAULT '', applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
					t.Fatal(err)
				}
				if _, err = admin.Exec(`INSERT INTO `+schema+`.schema_migrations(version,checksum) VALUES($1,'synthetic-mismatch')`, entries[0]); err != nil {
					t.Fatal(err)
				}
			}
			query := parsed.Query()
			query.Set("search_path", schema+",pg_catalog,public")
			parsed.RawQuery = query.Encode()
			db, err := sql.Open("pgx", parsed.String())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			var pid int
			if err = db.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- Run(context.Background(), db) }()
			select {
			case err = <-done:
				if err == nil {
					t.Fatal("synthetic migration fault unexpectedly succeeded")
				}
			case <-time.After(7 * time.Second):
				_, _ = admin.Exec(`SELECT pg_terminate_backend($1)`, pid)
				<-done
				t.Fatal("migration cleanup exceeded seven seconds")
			}
			var locks int
			deadline := time.Now().Add(time.Second)
			for {
				err = admin.QueryRow(`SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND pid=$1`, pid).Scan(&locks)
				if err != nil || locks == 0 || time.Now().After(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err != nil || locks != 0 {
				t.Fatalf("discarded session retains %d locks: %v", locks, err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err = db.PingContext(ctx); err != nil {
				t.Fatalf("pool did not recover: %v", err)
			}
			var next int
			if err = db.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&next); err != nil {
				t.Fatal(err)
			}
			if next == pid {
				t.Fatal("uncertain lock session returned to pool")
			}
		})
	}
}
