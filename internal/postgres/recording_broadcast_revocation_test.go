package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/httpapi"
	"hhc/asset-api/internal/migrations"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func broadcastRevocationFixture(t *testing.T) (*sql.DB, *RecordingCaptureStore, *broadcastMemoryObjects, assets.RecordingCapture, assets.RecordingBroadcastRange) {
	t.Helper()
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	objects := &broadcastMemoryObjects{values: map[string][]byte{}}
	store := NewRecordingCaptureStore(db).WithBroadcastObjects(objects)
	now := time.Now()
	c := assets.RecordingCapture{ID: strings.Repeat("b", 32), ActorID: "revocation-actor", RecordingID: "11111111-1111-4111-8111-111111111111", CreateKey: "revocation-create", State: "uploading", CreatedAt: now, ExpiresAt: now.Add(time.Hour), BroadcastEpoch: 1, Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}
	if _, err := store.CreateCapture(ctx, c); err != nil {
		t.Fatal(err)
	}
	p := assets.RecordingBroadcastRange{RecordingID: c.RecordingID, Epoch: 1, RangeRevision: 3, StartSequence: boundary(8), EndSequenceExclusive: boundary(20), MemberState: "vod"}
	if _, err := store.SetBroadcastRange(ctx, c.ID, p); err != nil {
		t.Fatal(err)
	}
	return db, store, objects, c, p
}

func TestDeletedBroadcastRevocationConvergesThroughCMSRoute(t *testing.T) {
	ctx := context.Background()
	db, store, objects, c, p := broadcastRevocationFixture(t)
	if err := NewRecordingDeletionStore(db).DeleteRecording(ctx, c.RecordingID); err != nil {
		t.Fatal(err)
	}
	p.RangeRevision = 4
	p.Revoked = true
	p.MemberState = "blocked"
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	h := httpapi.New(nil, nil, map[string]bool{"hhc-web-api": true, "account-api": true}, true, "", httpapi.WorkloadAuthConfig{}, nil).
		WithRecordingCaptures(assets.NewRecordingCaptureService(store, nil, time.Now)).WithRecordingBroadcasts(store).Routes()
	for _, tc := range []struct {
		caller string
		status int
	}{{"account-api", 403}, {"hhc-web-api", 200}, {"hhc-web-api", 200}} {
		r := httptest.NewRequest("PUT", "/priv/recording-captures/"+c.ID+"/broadcast-range", strings.NewReader(string(body)))
		r.Header.Set("X-Internal-Caller-App-Id", tc.caller)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s revocation: HTTP %d, want %d: %s", tc.caller, w.Code, tc.status, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatal("revocation response cacheable")
		}
	}
	stored, err := store.GetBroadcastRange(ctx, c.ID)
	if err != nil || stored == nil || !stored.Revoked || stored.MemberState != "blocked" || stored.RangeRevision != 4 {
		t.Fatalf("durable deny policy: %+v %v", stored, err)
	}
	raw, err := objects.ReadBroadcastRange(ctx, c.ID)
	var remote assets.RecordingBroadcastRange
	if err != nil || json.Unmarshal(raw, &remote) != nil || !remote.Revoked || remote.MemberState != "blocked" || remote.RangeRevision != 4 {
		t.Fatalf("remote deny policy: %+v %v", remote, err)
	}
	current, err := store.GetCapture(ctx, c.ID)
	if err != nil || current.State != "expired" || current.TerminalReason == nil || *current.TerminalReason != "recording_deleted" {
		t.Fatalf("capture resurrected: %+v %v", current, err)
	}
	projection, err := store.GetBroadcastProjection(ctx, c.ID)
	if err != nil || projection.State != "failed" {
		t.Fatalf("revoked projection: %+v %v", projection, err)
	}
	c.ID = strings.Repeat("c", 32)
	c.CreateKey = "late-revocation-create"
	c.BroadcastEpoch = 2
	if _, err = store.CreateCapture(ctx, c); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("revocation reopened creation: %v", err)
	}
}

func TestDeletedBroadcastRevocationKeepsAuthorityFences(t *testing.T) {
	ctx := context.Background()
	db, store, objects, c, old := broadcastRevocationFixture(t)
	if err := NewRecordingDeletionStore(db).DeleteRecording(ctx, c.RecordingID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*assets.RecordingBroadcastRange)
	}{
		{"nonrevoked", func(p *assets.RecordingBroadcastRange) { p.Revoked = false }},
		{"live", func(p *assets.RecordingBroadcastRange) { p.MemberState = "live" }},
		{"vod", func(p *assets.RecordingBroadcastRange) { p.MemberState = "vod" }},
		{"wrong recording", func(p *assets.RecordingBroadcastRange) { p.RecordingID = "22222222-2222-4222-8222-222222222222" }},
		{"wrong epoch", func(p *assets.RecordingBroadcastRange) { p.Epoch = 2 }},
		{"stale revision", func(p *assets.RecordingBroadcastRange) { p.RangeRevision = 2 }},
		{"equal revision different body", func(p *assets.RecordingBroadcastRange) { p.RangeRevision = 3 }},
		{"changed start", func(p *assets.RecordingBroadcastRange) { p.StartSequence = boundary(9) }},
		{"changed end", func(p *assets.RecordingBroadcastRange) { p.EndSequenceExclusive = boundary(21) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := old
			next.RangeRevision, next.Revoked, next.MemberState = 4, true, "blocked"
			tc.change(&next)
			if _, err := store.SetBroadcastRange(ctx, c.ID, next); !errors.Is(err, assets.ErrConflict) {
				t.Fatalf("invalid revocation accepted: %v", err)
			}
			got, err := store.GetBroadcastRange(ctx, c.ID)
			if err != nil || !reflect.DeepEqual(got, &old) {
				t.Fatalf("rejected policy changed DB: %+v %v", got, err)
			}
			raw, err := objects.ReadBroadcastRange(ctx, c.ID)
			var remote assets.RecordingBroadcastRange
			if err != nil || json.Unmarshal(raw, &remote) != nil || !reflect.DeepEqual(remote, old) {
				t.Fatalf("rejected policy changed provider: %+v %v", remote, err)
			}
		})
	}
	next := old
	next.RangeRevision, next.Revoked, next.MemberState = 4, true, "blocked"
	if _, err := store.SetBroadcastRange(ctx, strings.Repeat("f", 32), next); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("missing capture accepted: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.SetBroadcastRange(cancelled, c.ID, next); !errors.Is(err, context.Canceled) {
		t.Fatalf("storage cancellation changed to a business conflict: %v", err)
	}
	if _, err := store.SetBroadcastRange(ctx, c.ID, next); err != nil {
		t.Fatal(err)
	}
	next.RangeRevision, next.Revoked = 5, false
	if _, err := store.SetBroadcastRange(ctx, c.ID, next); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("unrevocation accepted: %v", err)
	}
}

type revocationHookObjects struct {
	*broadcastMemoryObjects
	afterPut func(context.Context)
}

func (o *revocationHookObjects) PutBroadcastRange(ctx context.Context, id string, data []byte) error {
	if err := o.broadcastMemoryObjects.PutBroadcastRange(ctx, id, data); err != nil {
		return err
	}
	if o.afterPut != nil {
		o.afterPut(ctx)
	}
	return nil
}

func TestDeletedBroadcastRevocationRetriesFailedAuthorityCommit(t *testing.T) {
	ctx := context.Background()
	db, store, objects, c, p := broadcastRevocationFixture(t)
	if err := NewRecordingDeletionStore(db).DeleteRecording(ctx, c.RecordingID); err != nil {
		t.Fatal(err)
	}
	p.RangeRevision, p.Revoked, p.MemberState = 4, true, "blocked"
	objects.fail = true
	if _, err := store.SetBroadcastRange(ctx, c.ID, p); err == nil {
		t.Fatal("provider failure acknowledged")
	}
	got, err := store.GetBroadcastRange(ctx, c.ID)
	if err != nil || got.Revoked || got.RangeRevision != 3 {
		t.Fatalf("provider failure committed DB: %+v %v", got, err)
	}
	objects.fail = false
	if _, err = db.ExecContext(ctx, `CREATE FUNCTION reject_authority_commit() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'injected authority commit failure'; END $$;
 CREATE CONSTRAINT TRIGGER reject_authority_commit AFTER UPDATE ON recording_broadcast_ranges
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_authority_commit()`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SetBroadcastRange(ctx, c.ID, p); err == nil || !strings.Contains(err.Error(), "injected authority commit failure") {
		t.Fatalf("deferred DB commit failure not propagated: %v", err)
	}
	got, err = store.GetBroadcastRange(ctx, c.ID)
	if err != nil || got.Revoked || got.RangeRevision != 3 {
		t.Fatalf("failed commit changed DB: %+v %v", got, err)
	}
	raw, err := objects.ReadBroadcastRange(ctx, c.ID)
	var remote assets.RecordingBroadcastRange
	if err != nil || json.Unmarshal(raw, &remote) != nil || !remote.Revoked || remote.RangeRevision != 4 {
		t.Fatalf("failed DB commit restored remote access: %+v %v", remote, err)
	}
	if _, err = db.ExecContext(ctx, `DROP TRIGGER reject_authority_commit ON recording_broadcast_ranges; DROP FUNCTION reject_authority_commit()`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = store.SetBroadcastRange(ctx, c.ID, p); err != nil {
			t.Fatalf("same denied authority did not converge: %v", err)
		}
	}
	got, err = store.GetBroadcastRange(ctx, c.ID)
	if err != nil || !reflect.DeepEqual(got, &p) {
		t.Fatalf("retry did not commit: %+v %v", got, err)
	}
}

func TestBroadcastRevocationAndDeletionSerialize(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, store, objects, c, p := broadcastRevocationFixture(t)
	var schema string
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	config, err := pgx.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = schema
	deleteDB := stdlib.OpenDB(*config)
	deleteDB.SetMaxOpenConns(1)
	t.Cleanup(func() { deleteDB.Close() })
	var deletePID int
	if err = deleteDB.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&deletePID); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	store.WithBroadcastObjects(&revocationHookObjects{objects, func(ctx context.Context) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}})
	p.RangeRevision, p.Revoked, p.MemberState = 4, true, "blocked"
	revoked, deleted := make(chan error, 1), make(chan error, 1)
	go func() { _, err := store.SetBroadcastRange(ctx, c.ID, p); revoked <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("authority write not reached")
	}
	go func() { deleted <- NewRecordingDeletionStore(deleteDB).DeleteRecording(ctx, c.RecordingID) }()
	for {
		var waiting bool
		if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')`, deletePID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err = <-deleted:
			t.Fatalf("deletion crossed the in-flight authority fence: %v", err)
		case <-ctx.Done():
			t.Fatal("deletion never reached owner lock")
		case <-time.After(10 * time.Millisecond):
		}
	}
	unblock()
	if err = <-revoked; err != nil {
		t.Fatal(err)
	}
	if err = <-deleted; err != nil {
		t.Fatal(err)
	}
	got, err := store.GetBroadcastRange(ctx, c.ID)
	if err != nil || !got.Revoked || got.RangeRevision != 4 {
		t.Fatalf("deletion lost revocation: %+v %v", got, err)
	}
	current, err := store.GetCapture(ctx, c.ID)
	if err != nil || current.State != "expired" || current.TerminalAt == nil {
		t.Fatalf("revocation lost deletion: %+v %v", current, err)
	}
}
