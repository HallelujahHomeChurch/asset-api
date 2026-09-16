package auditoutbox

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"hhc/asset-api/internal/auditclient"
	"hhc/asset-api/internal/migrations"
)

func TestEnqueueClaimAndFencedCompletion(t *testing.T) {
	db := isolatedDB(t)
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	event, err := auditclient.NewEvent("asset.collection.read", "0123456789abcdef0123456789abcdef", "user", "018f2f5b-8b6d-4a7d-9f2e-7a6b5c4d3e2f", "request-1", now, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := New(db)
	if err := store.Enqueue(context.Background(), event, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Enqueue(context.Background(), event, now.Add(time.Second)); err != nil {
		t.Fatalf("replay: %v", err)
	}
	row, err := store.Claim(context.Background(), now)
	if err != nil || row == nil || row.Attempts != 1 {
		t.Fatalf("claim=%#v err=%v", row, err)
	}
	stale := *row
	stale.Attempts++
	if updated, err := store.Complete(context.Background(), stale, "delivered", "", now, now); err != nil || updated {
		t.Fatalf("stale updated=%v err=%v", updated, err)
	}
	if updated, err := store.Complete(context.Background(), *row, "delivered", "", now, now); err != nil || !updated {
		t.Fatalf("updated=%v err=%v", updated, err)
	}
}

type fakeRepository struct {
	row       *Row
	completed string
}

func (f *fakeRepository) Claim(context.Context, time.Time) (*Row, error) {
	row := f.row
	f.row = nil
	return row, nil
}
func (f *fakeRepository) Complete(_ context.Context, _ Row, status, _ string, _ time.Time, _ time.Time) (bool, error) {
	f.completed = status
	return true, nil
}
func (*fakeRepository) Stats(context.Context, time.Time) (auditclient.PipelineStats, error) {
	return auditclient.PipelineStats{}, nil
}

type fakeAppender struct{ result string }

func (f fakeAppender) Append(context.Context, []byte) auditclient.AppendResult {
	return auditclient.AppendResult{Result: f.result, HTTPStatus: 201}
}

func TestWorkerDeliversOnlyCanonicalRows(t *testing.T) {
	event, err := auditclient.NewEvent("asset.collection.read", "0123456789abcdef0123456789abcdef", "user", "018f2f5b-8b6d-4a7d-9f2e-7a6b5c4d3e2f", "request-1", time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	payload, hash, err := event.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	repo := &fakeRepository{row: &Row{ID: "row", EventID: event.EventID, Payload: payload, PayloadHash: hash, Attempts: 1}}
	if err := NewWorker(repo, fakeAppender{"accepted"}).ProcessOnce(context.Background()); err != nil || repo.completed != "delivered" {
		t.Fatalf("status=%s err=%v", repo.completed, err)
	}
	repo = &fakeRepository{row: &Row{ID: "row", EventID: event.EventID, Payload: payload, PayloadHash: "corrupt", Attempts: 1}}
	if err := NewWorker(repo, fakeAppender{"accepted"}).ProcessOnce(context.Background()); err != nil || repo.completed != "dead_letter" {
		t.Fatalf("status=%s err=%v", repo.completed, err)
	}
}

func isolatedDB(t *testing.T) *sql.DB {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	config, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = admin.Close() })
	schema := fmt.Sprintf("asset_audit_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	testConfig := config.Copy()
	testConfig.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*testConfig)
	t.Cleanup(func() { _ = db.Close() })
	return db
}
