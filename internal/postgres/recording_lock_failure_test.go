package postgres

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRecordingLockAcquisitionFailureDiscardsSession(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("disposable PostgreSQL required")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost") {
		t.Fatal("loopback test database required")
	}
	observer, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	for _, fault := range []string{"live-worker", "session-policy", "session-owner", "session-owner-shared"} {
		t.Run(fault, func(t *testing.T) {
			db := isolatedIntegrationDB(t)
			db.SetMaxOpenConns(1)
			var schema string
			if err := db.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
				t.Fatal(err)
			}
			fn := "pg_try_advisory_lock"
			key := `hashtextextended('recording-live-worker',0)`
			if fault == "session-policy" {
				fn = "pg_try_advisory_lock_shared"
				key = retentionLock
			}
			if fault == "session-owner" || fault == "session-owner-shared" {
				key = `hashtextextended('recording-session:acquisition-fault',0)`
			}
			if fault == "session-owner-shared" {
				fn = "pg_try_advisory_lock_shared"
			}
			// Session locks survive a statement error, including an uncertain acquisition result.
			if _, err := db.Exec(`CREATE FUNCTION ` + fn + `(bigint) RETURNS boolean LANGUAGE plpgsql AS $$ BEGIN IF $1 = ` + key + ` THEN PERFORM pg_catalog.` + fn + `($1); RAISE EXCEPTION 'synthetic acquisition failure'; END IF; RETURN pg_catalog.` + fn + `($1); END $$`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`SELECT set_config('search_path',$1,false)`, schema+",pg_catalog"); err != nil {
				t.Fatal(err)
			}
			var pid int
			if err := db.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			var acquireErr error
			if fault == "live-worker" {
				_, acquireErr = NewRecordingCaptureStore(db).WithLiveWorker(context.Background(), func(context.Context, func(context.Context) error) error {
					t.Fatal("failed acquisition ran worker")
					return nil
				})
			} else if fault == "session-owner-shared" {
				acquireErr = newRecordingSessionStore(db).WithReadSessionLock(context.Background(), "acquisition-fault", func(context.Context, *sql.Conn) error { t.Fatal("failed acquisition ran callback"); return nil })
			} else {
				acquireErr = newRecordingSessionStore(db).WithSessionLock(context.Background(), "acquisition-fault", func(context.Context, *sql.Conn) error { t.Fatal("failed acquisition ran callback"); return nil })
			}
			if acquireErr == nil || !strings.Contains(acquireErr.Error(), "synthetic acquisition failure") {
				t.Fatalf("fault not exercised: %v", acquireErr)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var locks int
			deadline := time.Now().Add(500 * time.Millisecond)
			for {
				if err := observer.QueryRowContext(ctx, `SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND pid=$1`, pid).Scan(&locks); err != nil {
					t.Fatal(err)
				}
				if locks == 0 || time.Now().After(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if locks != 0 {
				t.Fatalf("uncertain session %d retains %d locks", pid, locks)
			}
			contender, err := observer.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer contender.Close()
			var acquired bool
			if err := contender.QueryRowContext(ctx, `SELECT pg_catalog.pg_try_advisory_lock(`+key+`)`).Scan(&acquired); err != nil || !acquired {
				t.Fatalf("different session cannot acquire: %v %v", acquired, err)
			}
			defer contender.ExecContext(context.Background(), `SELECT pg_catalog.pg_advisory_unlock(`+key+`)`)
			var next int
			if err := db.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&next); err != nil {
				t.Fatal(err)
			}
			if next == pid {
				t.Fatal("uncertain physical session reused")
			}
		})
	}
}
