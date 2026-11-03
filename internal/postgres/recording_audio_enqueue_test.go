package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hhc/asset-api/internal/migrations"
	"sync"
	"testing"
	"time"
)

func seedHistoricalAudio(t *testing.T, db *sql.DB, prefix string, count int) {
	t.Helper()
	p := testRecordingPackage(t, "fixture", "actor", "recording")
	inventory, err := json.Marshal(p.Inventory)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO recording_packages(id,session_id,owner_service,actor_id,recording_id,idempotency_key,state,size_bytes,inventory,created_at,expires_at,completed_at,ready_at,media_expires_at,final_prefix)
 SELECT $1||n,$1||n,'hhc-web-api','actor',$1||n,$1||n,'ready',$2,$3,now(),now()+interval '24 hours',now(),now(),now()+interval '30 days','recordings/packages/'||$1||n||'/final/validated/' FROM generate_series(1,$4::int)n`, prefix, p.SizeBytes, inventory, count)
	if err != nil {
		t.Fatal(err)
	}
}
func TestAudioEnqueueBoundedResumablePreservesExistingStates(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingPackageStore(db)
	seedHistoricalAudio(t, db, "old-", 254)
	for i, state := range []string{"pending", "processing", "ready", "failed"} {
		if _, err := db.Exec(`INSERT INTO recording_audio(package_id,version,source_prefix,source_digest,state,attempts) SELECT id,'aac-hls-v1',final_prefix,inventory->>'inventoryDigest',$2,3 FROM recording_packages WHERE id=$1`, fmt.Sprintf("old-%d", i+251), state); err != nil {
			t.Fatal(err)
		}
	}
	before, err := store.PlanRecordingAudio(ctx)
	if err != nil || before.Packages != 250 {
		t.Fatal(before, err)
	}
	for _, want := range []int64{100, 100, 50, 0} {
		n, err := store.EnqueueRecordingAudio(ctx, 100)
		if err != nil || n != want {
			t.Fatalf("enqueue=%d want=%d error=%v", n, want, err)
		}
	}
	after, err := store.PlanRecordingAudio(ctx)
	if err != nil || after.Packages != 0 {
		t.Fatal(after, err)
	}
	for i, state := range []string{"pending", "processing", "ready", "failed"} {
		var actual string
		var attempts int
		if err := db.QueryRow(`SELECT state,attempts FROM recording_audio WHERE package_id=$1`, fmt.Sprintf("old-%d", i+251)).Scan(&actual, &attempts); err != nil || actual != state || attempts != 3 {
			t.Fatalf("existing state reset: %s %d %v", actual, attempts, err)
		}
	}
}
func TestAudioEnqueueConcurrentBatchesDoNotDuplicate(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingPackageStore(db)
	seedHistoricalAudio(t, db, "parallel-", 201)
	type result struct {
		count int64
		err   error
	}
	done := make(chan result, 2)
	for range 2 {
		go func() { n, err := store.EnqueueRecordingAudio(ctx, 100); done <- result{n, err} }()
	}
	for range 2 {
		r := <-done
		if r.err != nil || r.count != 100 {
			t.Fatal(r)
		}
	}
	plan, err := store.PlanRecordingAudio(ctx)
	if err != nil || plan.Packages != 1 {
		t.Fatal(plan, err)
	}
	n, err := store.EnqueueRecordingAudio(ctx, 100)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
func TestAudioEnqueueRejectsExpiredDeletedAndInvalidSources(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingPackageStore(db)
	seedHistoricalAudio(t, db, "eligibility-", 5)
	for _, query := range []string{
		`UPDATE recording_packages SET media_expires_at=now()-interval '1 second' WHERE id='eligibility-1'`,
		`INSERT INTO recording_deletions(recording_id) VALUES('eligibility-2')`,
		`UPDATE recording_packages SET final_prefix='invalid' WHERE id='eligibility-3'`,
		`UPDATE recording_packages SET inventory=jsonb_set(inventory,'{inventoryDigest}','"invalid"') WHERE id='eligibility-4'`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := store.PlanRecordingAudio(ctx)
	if err != nil || plan.Packages != 1 {
		t.Fatal(plan, err)
	}
	n, err := store.EnqueueRecordingAudio(ctx, 100)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
func TestAudioEnqueueRechecksExpiryAfterPackageLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingPackageStore(db)
	seedHistoricalAudio(t, db, "expiring-", 1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE recording_packages SET media_expires_at=now()-interval '1 second' WHERE id='expiring-1'`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		n, err := store.EnqueueRecordingAudio(ctx, 100)
		if err == nil && n != 0 {
			err = fmt.Errorf("expired candidate enqueued: %d", n)
		}
		done <- err
	}()
	for {
		var waiting bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT p.id FROM recording_packages p WHERE p.id=%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("enqueue did not wait for source row: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAudioEnqueueRechecksRevocationAfterAuthorityFence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, captures, objects, c, policy := broadcastRevocationFixture(t)
	seedHistoricalAudio(t, db, "revoking-", 1)
	if _, err := db.Exec(`UPDATE recording_packages SET id=$1,session_id=$1,recording_id=$2,final_prefix='recordings/packages/'||$1||'/final/validated/' WHERE id='revoking-1'`, c.ID, c.RecordingID); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	captures.WithBroadcastObjects(&revocationHookObjects{objects, func(ctx context.Context) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}})
	policy.RangeRevision++
	policy.Revoked = true
	policy.MemberState = "blocked"
	revoked, applied := make(chan error, 1), make(chan error, 1)
	go func() { _, err := captures.SetBroadcastRange(ctx, c.ID, policy); revoked <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	go func() {
		n, err := NewRecordingPackageStore(db).EnqueueRecordingAudio(ctx, 100)
		if err == nil && n != 0 {
			err = fmt.Errorf("revoked candidate enqueued: %d", n)
		}
		applied <- err
	}()
	for {
		var waiting bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE '%recording-owner:%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-applied:
			t.Fatalf("enqueue crossed authority fence: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	unblock()
	if err := <-revoked; err != nil {
		t.Fatal(err)
	}
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
	plan, err := NewRecordingPackageStore(db).PlanRecordingAudio(ctx)
	if err != nil || plan.Packages != 0 {
		t.Fatal(plan, err)
	}
}

func TestAudioEnqueueSmallBatchAndInvalidLimit(t *testing.T) {
	ctx := context.Background()
	for _, limit := range []int{-1, 0, 101} {
		if n, err := NewRecordingPackageStore(nil).EnqueueRecordingAudio(ctx, limit); err == nil || n != 0 {
			t.Fatalf("invalid limit %d: %d %v", limit, n, err)
		}
	}
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingPackageStore(db)
	seedHistoricalAudio(t, db, "pilot-", 3)
	for _, remaining := range []int64{2, 1, 0} {
		n, err := store.EnqueueRecordingAudio(ctx, 1)
		if err != nil || n != 1 {
			t.Fatalf("pilot enqueue=%d error=%v", n, err)
		}
		plan, err := store.PlanRecordingAudio(ctx)
		if err != nil || plan.Packages != remaining {
			t.Fatal(plan, err)
		}
	}
}
