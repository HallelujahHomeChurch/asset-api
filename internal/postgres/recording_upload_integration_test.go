package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
)

func recordingSession(id, admin, recording string) assets.RecordingUploadSession {
	now := time.Now().UTC().Truncate(time.Second)
	return assets.RecordingUploadSession{
		ID: id, AssetVersionID: "version-" + id, OwnerService: "hhc-web-api", AdminID: admin,
		RecordingID: recording, FileName: "meeting.mp4", ObjectKey: "recordings/version-" + id + ".mp4",
		UploadID: "r2-" + id, IdempotencyKey: "idem-" + id, Status: "created",
		SizeBytes: 1, ChecksumSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour),
	}
}

func TestRecordingUploadPersistenceAndCAS(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingUploadStore(db)
	session := recordingSession("session-a", "admin-a", "recording-a")
	if err := store.Create(ctx, session); err != nil {
		t.Fatal(err)
	}
	got, err := store.FindByIdempotency(ctx, session.IdempotencyKey)
	if err != nil || got.ID != session.ID || got.SizeBytes != session.SizeBytes {
		t.Fatalf("lookup: %+v, %v", got, err)
	}
	if err := store.ClaimCompletion(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimCompletion(ctx, session.ID); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("second claim = %v", err)
	}
	if err := store.MarkValidating(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(ctx, session.ID)
	if err != nil || got.Status != "validating" {
		t.Fatalf("validating: %+v, %v", got, err)
	}
	if err := store.ClaimAbandonedDeletion(ctx, session.ID, time.Now().Add(48*time.Hour)); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("cleanup claimed completed upload: %v", err)
	}
}

func TestRecordingSessionLockExcludesOtherProcessAndReleases(t *testing.T) {
	db := isolatedIntegrationDB(t)
	first, second := NewRecordingUploadStore(db), NewRecordingUploadStore(db)
	ctx := context.Background()
	err := first.WithSessionLock(ctx, "same-session", func(ctx context.Context) error {
		err := second.WithSessionLock(ctx, "same-session", func(context.Context) error { t.Error("concurrent mutation entered"); return nil })
		if !errors.Is(err, assets.ErrConflict) {
			t.Fatalf("expected lock conflict: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.WithSessionLock(ctx, "same-session", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("lock leaked: %v", err)
	}
}

func TestRecordingSessionLockUsesOneConnectionAndPersistsTransitions(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingUploadStore(db)
	session := recordingSession("one-connection", "admin", "recording")
	if err := store.Create(ctx, session); err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	lockCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	want := errors.New("simulate R2 interruption after durable claim")
	err := store.WithSessionLock(lockCtx, session.ID, func(ctx context.Context) error {
		if _, err := store.Get(ctx, session.ID); err != nil {
			return err
		}
		if err := store.ClaimCompletion(ctx, session.ID); err != nil {
			return err
		}
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("locked operation = %v, want simulated interruption", err)
	}
	got, err := store.Get(ctx, session.ID)
	if err != nil || got.Status != "completing" {
		t.Fatalf("claim must survive callback failure: status=%q err=%v", got.Status, err)
	}
}

func TestRecordingUploadLimitsAreAtomic(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingUploadStore(db)
	var group sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			results <- store.Create(ctx, recordingSession(fmt.Sprintf("parallel-%d", i), "same-admin", "same-recording"))
		}(i)
	}
	group.Wait()
	close(results)
	created, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			created++
		case errors.Is(err, assets.ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected create error: %v", err)
		}
	}
	if created != 1 || conflicts != 7 {
		t.Fatalf("created=%d conflicts=%d", created, conflicts)
	}
}

func TestRecordingUploadRecordingFileLimit(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingUploadStore(db)
	for i := 0; i < 1; i++ {
		if err := store.Create(ctx, recordingSession(fmt.Sprintf("file-%d", i), fmt.Sprintf("admin-%d", i), "same-recording")); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Create(ctx, recordingSession("file-four", "admin-four", "same-recording")); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("fourth file = %v", err)
	}
}

func TestRecordingCompletionTimestampSurvivesReplay(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingUploadStore(db)
	session := recordingSession("timestamp", "admin", "recording")
	if err := store.Create(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimCompletion(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkValidating(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	first, err := store.Get(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(first)
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["uploadedAt"] == nil {
		t.Fatal("completed session must retain its authoritative uploadedAt")
	}
	if err := store.MarkValidating(ctx, session.ID); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("replayed transition: %v", err)
	}
	reloaded, err := NewRecordingUploadStore(db).GetByVersion(ctx, session.AssetVersionID)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(reloaded)
	if string(encoded) != string(again) {
		t.Fatal("completion replay changed persisted metadata")
	}
}

func TestRecordingOneSessionPerRecordingAfterDeletion(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingUploadStore(db)
	first := recordingSession("first", "admin", "immutable")
	if err := store.Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimDeletion(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDeleted(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, recordingSession("replacement", "other-admin", "immutable")); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("replacement after deletion: %v", err)
	}
	if err := store.Create(ctx, recordingSession("new-recording", "admin", "new-recording")); err != nil {
		t.Fatalf("new recording rejected: %v", err)
	}
}

func TestRecordingValidationClaimAndReady(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingUploadStore(db)
	session := recordingSession("validate-a", "admin-a", "recording-a")
	if err := store.Create(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimCompletion(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkValidating(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimValidation(ctx, time.Now(), time.Hour)
	if err != nil || claim.Session.ID != session.ID || claim.ClaimID == "" {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	if _, err := store.ClaimValidation(ctx, time.Now(), time.Hour); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("duplicate claim=%v", err)
	}
	if err := store.MarkRecordingReady(ctx, session.ID, "wrong", 9000); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("stale ready=%v", err)
	}
	if err := store.MarkRecordingReady(ctx, session.ID, claim.ClaimID, 9000); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, session.ID)
	if err != nil || got.Status != "ready" {
		t.Fatalf("ready=%+v err=%v", got, err)
	}
}

func TestRecordingValidationThirdLostClaimBecomesFailed(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingUploadStore(db)
	session := recordingSession("lost-third-claim", "admin", "recording")
	if err := store.Create(ctx, session); err != nil {
		t.Fatal(err)
	}
	if err := store.ClaimCompletion(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkValidating(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for attempt := 0; attempt < 3; attempt++ {
		claim, err := store.ClaimValidation(ctx, now.Add(time.Duration(attempt)*21*time.Minute), 20*time.Minute)
		if err != nil || claim.Attempts != attempt+1 {
			t.Fatalf("attempt %d: claim=%+v err=%v", attempt+1, claim, err)
		}
	}
	if _, err := store.ClaimValidation(ctx, now.Add(63*time.Minute), 20*time.Minute); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("exhausted claim = %v", err)
	}
	got, err := store.Get(ctx, session.ID)
	if err != nil || got.Status != "failed" {
		t.Fatalf("lost third claim status=%q err=%v", got.Status, err)
	}
}
