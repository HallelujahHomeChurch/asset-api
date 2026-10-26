package postgres

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
)

func observerScript(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../scripts/sql/maintenance-observer.sql")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func runObserver(t *testing.T, db *sql.DB, script string) (map[string][]string, error) {
	t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	result := map[string][]string{}
	err = conn.Raw(func(driver any) error {
		pg := driver.(*stdlib.Conn).Conn().PgConn()
		batches, err := pg.Exec(context.Background(), script).ReadAll()
		if err != nil {
			_, _ = pg.Exec(context.Background(), "ROLLBACK").ReadAll()
			return err
		}
		for _, batch := range batches {
			if len(batch.FieldDescriptions) == 0 {
				continue
			}
			names := make([]string, len(batch.FieldDescriptions))
			for i, f := range batch.FieldDescriptions {
				names[i] = f.Name
			}
			result["columns"] = names
			for _, row := range batch.Rows {
				values := make([]string, len(row))
				for i, v := range row {
					values[i] = string(v)
				}
				result[values[0]] = values
			}
		}
		return nil
	})
	return result, err
}
func observerDB(t *testing.T) *sql.DB {
	t.Helper()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}
func TestMaintenanceObserverClassifiesRealPostgresState(t *testing.T) {
	db := observerDB(t)
	script := observerScript(t)
	empty, err := runObserver(t, db, script)
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"component", "leased", "expired_claim_review", "unleased_effect_review", "terminal_failed_candidate", "upload_session_review", "retained_delayed_candidate", "retained_due_candidate", "max_lease_until", "next_due_at", "earliest_expiry_at", "last_attempt_started_at", "observed_at"}
	if strings.Join(empty["columns"], ",") != strings.Join(expected, ",") {
		t.Fatal("output allowlist changed", empty["columns"])
	}
	if len(empty) != 22 {
		t.Fatalf("missing fixed components: %d", len(empty))
	}
	for key, row := range empty {
		if key == "columns" {
			continue
		}
		for _, v := range row[1:8] {
			if v != "0" {
				t.Fatalf("nonempty clean component %s: %v", key, row)
			}
		}
	}
	now := time.Now().UTC()
	for _, id := range []string{"secret-active", "secret-expired", "secret-due", "secret-delayed", "secret-failed"} {
		scan, processing := assets.ScanClean, assets.ProcessingPending
		if id == "secret-active" || id == "secret-expired" {
			scan, processing = assets.ScanPending, assets.ProcessingReady
		}
		if id == "secret-failed" {
			processing = assets.ProcessingFailed
		}
		insertAsset(t, db, id, assets.UploadCompleted, scan, processing, now, time.Time{})
	}
	for _, q := range []string{
		"UPDATE assets SET scan_claimed_until=now()+interval '1 minute' WHERE id='secret-active'",
		"UPDATE assets SET scan_claimed_until=now()-interval '1 minute' WHERE id='secret-expired'",
		"UPDATE assets SET processing_next_attempt_at=now()+interval '1 hour' WHERE id='secret-delayed'",
		"INSERT INTO asset_derivative_outbox(event_id,asset_id,asset_etag,available_at,created_at) VALUES('secret-event','secret-due','secret-etag',now()+interval '1 hour',now())",
		"UPDATE recording_event_wakes SET state='running',token='secret-token',dispatch_generation=generation,lease_until=now()+interval '1 minute' WHERE lane='live'",
		`INSERT INTO recording_packages(id,session_id,owner_service,actor_id,recording_id,idempotency_key,state,size_bytes,inventory,created_at,expires_at,completed_at) VALUES('secret-package','secret-package','hhc-web-api','secret-actor','secret-recording','secret-key','validating',1,'{}',now(),now()+interval '1 hour',now())`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := runObserver(t, db, script)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ component, column, want string }{
		{"scan", "leased", "1"}, {"scan", "expired_claim_review", "1"},
		{"derivative", "retained_due_candidate", "1"}, {"derivative", "retained_delayed_candidate", "1"}, {"derivative", "terminal_failed_candidate", "1"},
		{"derivative_outbox", "retained_delayed_candidate", "1"}, {"wake_live", "leased", "1"}, {"package", "unleased_effect_review", "1"},
	} {
		for i, col := range rows["columns"] {
			if col == tc.column && rows[tc.component][i] != tc.want {
				t.Fatalf("%s %s: %v", tc.component, col, rows[tc.component])
			}
		}
	}
	for _, row := range rows {
		if strings.Contains(strings.Join(row, "|"), "secret-") {
			t.Fatal("private fixture value escaped")
		}
	}
	if rows["scan"][11] != "" {
		t.Fatal("invented last claim timestamp")
	}
	if _, err := db.Exec("UPDATE assets SET processing_status='ready' WHERE id='secret-due'"); err != nil {
		t.Fatal(err)
	}
	next, err := runObserver(t, db, script)
	if err != nil {
		t.Fatal(err)
	}
	if next["derivative"][7] != "0" {
		t.Fatal("observation transaction remained stale")
	}
}

func TestMaintenanceObserverFailsClosedOnDatabaseErrors(t *testing.T) {
	db := observerDB(t)
	script := observerScript(t)
	var schema string
	if err := db.QueryRow("SELECT quote_ident(current_schema())").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	denied := strings.Replace(script, "FROM assets", "FROM "+schema+".assets", -1)
	for _, tc := range []struct{ name, script, code string }{
		{"read-only", strings.Replace(script, "WITH components", "DELETE FROM assets;\nWITH components", 1), "25006"},
		{"denied", strings.Replace(denied, "WITH components", "SET LOCAL ROLE pg_monitor;\nWITH components", 1), "42501"},
		{"statement-timeout", strings.Replace(strings.Replace(script, "statement_timeout = '5s'", "statement_timeout = '20ms'", 1), "WITH components", "SELECT pg_sleep(1);\nWITH components", 1), "57014"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := runObserver(t, db, tc.script)
			var pg *pgconn.PgError
			if !errors.As(err, &pg) || pg.Code != tc.code || len(rows) != 0 {
				t.Fatalf("must fail without zero inventory: rows=%d err=%v", len(rows), err)
			}
		})
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("LOCK TABLE assets IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	rows, err := runObserver(t, db, script)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "55P03" || len(rows) != 0 {
		t.Fatalf("lock timeout must fail: %v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("observer held long lock wait")
	}
}
