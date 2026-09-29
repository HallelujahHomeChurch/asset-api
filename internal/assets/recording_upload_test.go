package assets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"hhc/asset-api/internal/storage/r2"
)

type fakeRecordingRepo struct {
	sessions       map[string]RecordingUploadSession
	beforeDeletion func()
	completionTime time.Time
	markError      error
}

func (f *fakeRecordingRepo) WithSessionLock(ctx context.Context, _ string, run func(context.Context) error) error {
	return run(ctx)
}

func (f *fakeRecordingRepo) FindByIdempotency(_ context.Context, key string) (RecordingUploadSession, error) {
	for _, session := range f.sessions {
		if session.IdempotencyKey == key {
			return session, nil
		}
	}
	return RecordingUploadSession{}, ErrNotFound
}
func (f *fakeRecordingRepo) Create(_ context.Context, session RecordingUploadSession) error {
	f.sessions[session.ID] = session
	return nil
}
func (f *fakeRecordingRepo) Get(_ context.Context, id string) (RecordingUploadSession, error) {
	if session, ok := f.sessions[id]; ok {
		return session, nil
	}
	return RecordingUploadSession{}, ErrNotFound
}
func (f *fakeRecordingRepo) GetByVersion(_ context.Context, versionID string) (RecordingUploadSession, error) {
	for _, session := range f.sessions {
		if session.AssetVersionID == versionID {
			return session, nil
		}
	}
	return RecordingUploadSession{}, ErrNotFound
}
func (f *fakeRecordingRepo) MarkDeleted(_ context.Context, id string) error {
	session, ok := f.sessions[id]
	if !ok {
		return ErrNotFound
	}
	session.Status = "deleted"
	f.sessions[id] = session
	return nil
}
func (f *fakeRecordingRepo) ClaimDeletion(_ context.Context, id string) error {
	if f.beforeDeletion != nil {
		f.beforeDeletion()
	}
	session, ok := f.sessions[id]
	if !ok || session.Status == "deleted" {
		return ErrConflict
	}
	session.Status = "deleting"
	f.sessions[id] = session
	return nil
}
func (f *fakeRecordingRepo) MarkValidating(_ context.Context, id string) error {
	if f.markError != nil {
		return f.markError
	}
	session, ok := f.sessions[id]
	if !ok || session.Status != "completing" {
		return ErrConflict
	}
	session.Status = "validating"
	if !f.completionTime.IsZero() {
		at := f.completionTime
		session.UploadedAt = &at
	}
	f.sessions[id] = session
	return nil
}

func (f *fakeRecordingRepo) ClaimAbandonedDeletion(_ context.Context, id string, now time.Time) error {
	if f.beforeDeletion != nil {
		f.beforeDeletion()
	}
	s, ok := f.sessions[id]
	if !ok || (s.Status != "deleting" && (now.Before(s.ExpiresAt) || s.UploadedAt != nil || (s.Status != "created" && s.Status != "completing" && s.Status != "failed" && s.Status != "cancelled"))) {
		return ErrConflict
	}
	s.Status = "deleting"
	f.sessions[id] = s
	return nil
}
func (f *fakeRecordingRepo) ClaimCompletion(_ context.Context, id string) error {
	session, ok := f.sessions[id]
	if !ok || session.Status != "created" {
		return ErrConflict
	}
	session.Status = "completing"
	f.sessions[id] = session
	return nil
}
func (f *fakeRecordingRepo) MarkCancelled(_ context.Context, id string) error {
	session, ok := f.sessions[id]
	if !ok || session.Status != "created" {
		return ErrConflict
	}
	session.Status = "cancelled"
	f.sessions[id] = session
	return nil
}

type fakeRecordingStore struct {
	creates, aborts, completes int
	deletes                    int
	abortError                 error
	listError                  error
	headError                  error
	parts                      []r2.Part
	size                       int64
}

func (f *fakeRecordingStore) Delete(context.Context, string) error { f.deletes++; return nil }

func (f *fakeRecordingStore) Create(context.Context, string) (string, error) {
	f.creates++
	return "r2-upload-1", nil
}
func (f *fakeRecordingStore) Abort(context.Context, string, string) error { f.aborts++; return f.abortError }
func (f *fakeRecordingStore) PresignPart(context.Context, string, string, int, time.Duration) (r2.PresignedPart, error) {
	return r2.PresignedPart{URL: "https://example.invalid/signed", Method: "PUT"}, nil
}
func (f *fakeRecordingStore) ListParts(context.Context, string, string) ([]r2.Part, error) {
	return f.parts, f.listError
}
func (f *fakeRecordingStore) Complete(context.Context, string, string, []r2.Part) error {
	f.completes++
	return nil
}
func (f *fakeRecordingStore) Head(context.Context, string) (int64, string, error) {
	return f.size, `"etag"`, f.headError
}

func TestRecordingAbandonedCleanupRecoversCommittedObjectAfterSessionExpiry(t *testing.T) {
	now := time.Now().UTC()
	session := RecordingUploadSession{ID: "session", AssetVersionID: "version", OwnerService: "hhc-web-api", AdminID: "admin", Status: "completing", SizeBytes: 10, ExpiresAt: now.Add(-time.Hour)}
	repo := &fakeRecordingRepo{sessions: map[string]RecordingUploadSession{"session": session}, completionTime: now}
	objects := &fakeRecordingStore{size: 10, listError: errors.New("multipart already completed")}
	service := NewRecordingUploadService(repo, objects, func() time.Time { return now })
	if err := service.DeleteAbandoned(context.Background(), "version", now); err != nil {
		t.Fatal(err)
	}
	if objects.deletes != 0 || repo.sessions["session"].Status != "validating" {
		t.Fatalf("committed object lost: deletes=%d status=%s", objects.deletes, repo.sessions["session"].Status)
	}
}

func TestRecordingDeletionRetriesFailedMultipartAbort(t *testing.T) {
	session := RecordingUploadSession{ID: "session", AssetVersionID: "version", OwnerService: "hhc-web-api", Status: "ready", ObjectKey: "recordings/version.mp4", UploadID: "upload"}
	repo := &fakeRecordingRepo{sessions: map[string]RecordingUploadSession{"session": session}}
	objects := &fakeRecordingStore{abortError: errors.New("R2 unavailable")}
	service := NewRecordingUploadService(repo, objects, time.Now)
	if err := service.DeleteAsset(context.Background(), "version"); err == nil {
		t.Fatal("failed multipart abort must be retried")
	}
	if repo.sessions["session"].Status != "deleting" || objects.deletes != 0 {
		t.Fatalf("failed abort lost retry state: status=%s deletes=%d", repo.sessions["session"].Status, objects.deletes)
	}
	objects.abortError = nil
	if err := service.DeleteAsset(context.Background(), "version"); err != nil {
		t.Fatal(err)
	}
	if repo.sessions["session"].Status != "deleted" || objects.aborts != 2 || objects.deletes != 1 {
		t.Fatalf("retry status=%s aborts=%d deletes=%d", repo.sessions["session"].Status, objects.aborts, objects.deletes)
	}
}

func TestRecordingSessionCreateReplayAndPartOwner(t *testing.T) {
	now := time.Date(2026, 9, 27, 13, 0, 0, 0, time.UTC)
	repo := &fakeRecordingRepo{sessions: map[string]RecordingUploadSession{}}
	store := &fakeRecordingStore{}
	service := NewRecordingUploadService(repo, store, func() time.Time { return now })
	checksum := hex.EncodeToString(make([]byte, 32))
	input := CreateRecordingUploadInput{AdminID: "admin-1", RecordingID: "rec-1", FileName: "meeting.mp4", SizeBytes: 10_000_000_000, ChecksumSHA256: checksum, IdempotencyKey: "idem-1"}
	session, err := service.Create(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if session.SizeBytes != 10_000_000_000 || session.ExpiresAt != now.Add(24*time.Hour) || session.ObjectKey == "" || store.creates != 1 {
		t.Fatalf("unexpected session: %+v, creates=%d", session, store.creates)
	}
	replayed, err := service.Create(context.Background(), input)
	if err != nil || replayed.ID != session.ID || store.creates != 1 {
		t.Fatalf("replay=%+v err=%v creates=%d", replayed, err, store.creates)
	}
	different := sha256.Sum256([]byte("different"))
	input.ChecksumSHA256 = hex.EncodeToString(different[:])
	if _, err := service.Create(context.Background(), input); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed bytes with same key = %v", err)
	}
	if _, err := service.SignPart(context.Background(), session.ID, "admin-2", 1); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other admin signed part: %v", err)
	}
	if _, err := service.SignPart(context.Background(), session.ID, "admin-1", 598); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("part 598: %v", err)
	}
	if _, err := service.SignPart(context.Background(), session.ID, "admin-1", 597); err != nil {
		t.Fatalf("last valid part: %v", err)
	}
	now = session.ExpiresAt
	if _, err := service.SignPart(context.Background(), session.ID, "admin-1", 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired session signed: %v", err)
	}
}

func TestRecordingUploadedPartsOnlyReturnsCompleteOwnedParts(t *testing.T) {
	now := time.Date(2026, 9, 27, 13, 0, 0, 0, time.UTC)
	repo := &fakeRecordingRepo{sessions: map[string]RecordingUploadSession{}}
	store := &fakeRecordingStore{parts: []r2.Part{{Number: 1, ETag: `"one"`, Size: 16 << 20}, {Number: 2, ETag: `"two"`, Size: 2}, {Number: 3, ETag: `"extra"`, Size: 1}}}
	service := NewRecordingUploadService(repo, store, func() time.Time { return now })
	session, err := service.Create(context.Background(), CreateRecordingUploadInput{AdminID: "admin-1", RecordingID: "rec-1", FileName: "meeting.mp4", SizeBytes: (16 << 20) + 1, ChecksumSHA256: hex.EncodeToString(make([]byte, 32)), IdempotencyKey: "idem-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.UploadedParts(context.Background(), session.ID, "admin-2"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other admin: %v", err)
	}
	numbers, err := service.UploadedParts(context.Background(), session.ID, "admin-1")
	if err != nil || len(numbers) != 1 || numbers[0] != 1 {
		t.Fatalf("parts=%v err=%v", numbers, err)
	}
}

func TestRecordingCompleteRejectsMissingOrWrongSizedParts(t *testing.T) {
	now := time.Date(2026, 9, 27, 13, 0, 0, 0, time.UTC)
	checksum := hex.EncodeToString(make([]byte, 32))
	for _, tt := range []struct {
		name    string
		parts   []r2.Part
		wantErr bool
	}{
		{"valid", []r2.Part{{Number: 1, ETag: `"a"`, Size: 16 << 20}, {Number: 2, ETag: `"b"`, Size: 1}}, false},
		{"missing", []r2.Part{{Number: 1, ETag: `"a"`, Size: 16 << 20}}, true},
		{"wrong first size", []r2.Part{{Number: 1, ETag: `"a"`, Size: (16 << 20) - 1}, {Number: 2, ETag: `"b"`, Size: 2}}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeRecordingRepo{sessions: map[string]RecordingUploadSession{}}
			store := &fakeRecordingStore{parts: tt.parts, size: (16 << 20) + 1}
			service := NewRecordingUploadService(repo, store, func() time.Time { return now })
			session, err := service.Create(context.Background(), CreateRecordingUploadInput{AdminID: "admin-1", RecordingID: "rec-1", FileName: "meeting.mp4", SizeBytes: (16 << 20) + 1, ChecksumSHA256: checksum, IdempotencyKey: "idem-1"})
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.Complete(context.Background(), session.ID, "admin-1")
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidUpload) || store.completes != 0 {
					t.Fatalf("invalid parts completed: result=%+v err=%v count=%d", result, err, store.completes)
				}
				return
			}
			if err != nil || result.Status != "validating" || store.completes != 1 {
				t.Fatalf("valid parts: result=%+v err=%v count=%d", result, err, store.completes)
			}
			replayed, err := service.Complete(context.Background(), session.ID, "admin-1")
			if err != nil || replayed.Status != "validating" || store.completes != 1 {
				t.Fatalf("completion replay: result=%+v err=%v count=%d", replayed, err, store.completes)
			}
		})
	}
}

func TestRecordingUploadLimits(t *testing.T) {
	for _, tt := range []struct {
		size  int64
		parts int
		ok    bool
	}{
		{1, 1, true},
		{16 << 20, 1, true},
		{(16 << 20) + 1, 2, true},
		{10_000_000_000, 597, true},
		{0, 0, false},
		{10_000_000_001, 0, false},
	} {
		parts, err := RecordingPartCount(tt.size)
		if (err == nil) != tt.ok || (tt.ok && parts != tt.parts) {
			t.Errorf("size %d: parts=%d err=%v", tt.size, parts, err)
		}
	}
}

func TestRecordingResumeRequiresExactManifest(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	first := sha256.Sum256([]byte("original bytes"))
	other := sha256.Sum256([]byte("different bytes"))
	session := RecordingUploadSession{
		OwnerService: "hhc-web-api", AdminID: "admin-1", RecordingID: "rec-1",
		SizeBytes: 14, ChecksumSHA256: hex.EncodeToString(first[:]),
		ExpiresAt: now.Add(24 * time.Hour),
	}
	if !session.CanResume("hhc-web-api", "admin-1", "rec-1", 14, hex.EncodeToString(first[:]), now) {
		t.Fatal("matching upload should resume")
	}
	if session.CanResume("hhc-web-api", "admin-1", "rec-1", 14, hex.EncodeToString(other[:]), now) {
		t.Fatal("same name and size with different bytes must not resume")
	}
	if session.CanResume("hhc-web-api", "admin-2", "rec-1", 14, hex.EncodeToString(first[:]), now) {
		t.Fatal("another admin must not resume")
	}
	if session.CanResume("hhc-web-api", "admin-1", "rec-1", 14, hex.EncodeToString(first[:]), session.ExpiresAt) {
		t.Fatal("expired session must not resume")
	}
}

func TestRecordingCompletionRecoversPersistedTimestamp(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC)
	repo := &fakeRecordingRepo{sessions: map[string]RecordingUploadSession{}, completionTime: now, markError: errors.New("DB unavailable")}
	objects := &fakeRecordingStore{parts: []r2.Part{{Number: 1, Size: 1, ETag: "part"}}, size: 1}
	svc := NewRecordingUploadService(repo, objects, func() time.Time { return now })
	session, err := svc.Create(ctx, CreateRecordingUploadInput{AdminID: "admin", RecordingID: "recording", FileName: "video.mp4", SizeBytes: 1, ChecksumSHA256: hex.EncodeToString(make([]byte, 32)), IdempotencyKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Complete(ctx, session.ID, "admin"); err == nil {
		t.Fatal("DB failure must not report completion")
	}
	repo.markError = nil
	objects.listError = errors.New("multipart no longer exists")
	// Reopening the editor only reads status; no original browser File remains.
	got, err := NewRecordingUploadService(repo, objects, func() time.Time { return now }).Get(ctx, session.ID, "admin")
	if err != nil || got.UploadedAt == nil || !got.UploadedAt.Equal(now) {
		t.Fatalf("recovered=%+v err=%v", got, err)
	}
	later := now.Add(72 * time.Hour)
	replayed, err := NewRecordingUploadService(repo, objects, func() time.Time { return later }).Complete(ctx, session.ID, "admin")
	if err != nil || replayed.UploadedAt == nil || !replayed.UploadedAt.Equal(now) || objects.completes != 1 {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
}

func TestRecordingAbandonedCleanupCannotDeleteConcurrentCompletion(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	repo := &fakeRecordingRepo{sessions: map[string]RecordingUploadSession{"upload": {ID: "upload", OwnerService: "hhc-web-api", AssetVersionID: "version", Status: "completing", ExpiresAt: now.Add(-time.Hour)}}}
	repo.beforeDeletion = func() {
		s := repo.sessions["upload"]
		s.Status = "validating"
		s.UploadedAt = &now
		repo.sessions["upload"] = s
	}
	objects := &fakeRecordingStore{headError: r2.ErrNotFound}
	err := NewRecordingUploadService(repo, objects, func() time.Time { return now }).DeleteAbandoned(ctx, "version", now)
	if !errors.Is(err, ErrConflict) || objects.deletes != 0 {
		t.Fatalf("completed file deleted: err=%v deletes=%d", err, objects.deletes)
	}
}
