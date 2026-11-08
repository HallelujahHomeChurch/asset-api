package telemetry

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"hhc/asset-api/internal/dbphase"
)

func localDiagnosticDatabase(dsn string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Database != "postgres" || cfg.Host != "localhost" && cfg.Host != "127.0.0.1" {
		return nil, errors.New("requires disposable loopback postgres database")
	}
	for _, fallback := range cfg.Fallbacks {
		if fallback.Host != "localhost" && fallback.Host != "127.0.0.1" {
			return nil, errors.New("requires loopback database fallbacks")
		}
	}
	return cfg, nil
}

func TestDiagnosticDatabaseRejectsRemoteFallback(t *testing.T) {
	dsn := "host=127.0.0.1,remote.invalid port=5432 dbname=postgres sslmode=disable"
	parsed, err := pgx.ParseConfig(dsn)
	if err != nil || len(parsed.Fallbacks) == 0 {
		t.Fatal("fixture must parse as a multi-host connection")
	}
	if _, err := localDiagnosticDatabase(dsn); err == nil {
		t.Fatal("remote fallback accepted")
	}
	if _, err := localDiagnosticDatabase("host=localhost,127.0.0.1 dbname=postgres sslmode=prefer"); err != nil {
		t.Fatal("loopback TLS/host fallbacks rejected")
	}
}

func TestRealDatabaseCancellationKeepsRequestCorrelation(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("requires disposable loopback TEST_POSTGRES_DSN")
	}
	cfg, err := localDiagnosticDatabase(dsn)
	if err != nil {
		t.Fatal(err)
	}
	db := dbphase.Open(*cfg, RecordDatabaseOutcome)
	defer db.Close()
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	logs := captureDiagnosticLogs(t)
	request := httptest.NewRequest("GET", "/priv/recording-packages/private-package", nil)
	request.Header.Set("X-HHC-Request-ID", "0123456789abcdef0123456789abcdef")
	response := httptest.NewRecorder()
	Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 200*time.Millisecond)
		defer cancel()
		if _, err := db.ExecContext(ctx, "SELECT pg_sleep(10)"); err == nil {
			t.Fatal("expected database cancellation")
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})).ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("changed HTTP result: %d", response.Code)
	}
	found := false
	scanner := bufio.NewScanner(strings.NewReader(logs.String()))
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event["phase"] == "db_query_lifecycle" {
			found = true
			if event["request_id"] != "0123456789abcdef0123456789abcdef" || event["route_id"] != "recording_package_read" || event["context_state"] != "deadline" {
				t.Fatalf("database failure lost request context: %s", scanner.Text())
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !found || strings.Contains(logs.String(), "pg_sleep") || strings.Contains(logs.String(), "private-package") {
		t.Fatalf("missing or unsafe database diagnostic: %s", logs.String())
	}
}
