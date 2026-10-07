package assets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type captureRepo struct {
	captures map[string]RecordingCapture
}

func (r *captureRepo) CreateCapture(_ context.Context, c RecordingCapture) (RecordingCapture, error) {
	for _, old := range r.captures {
		if old.CreateKey == c.CreateKey {
			return old, nil
		}
	}
	r.captures[c.ID] = c
	return c, nil
}
func (r *captureRepo) GetCapture(_ context.Context, id string) (RecordingCapture, error) {
	c, ok := r.captures[id]
	if !ok {
		return c, ErrNotFound
	}
	return c, nil
}
func (r *captureRepo) UpdateCapture(ctx context.Context, id string, fn func(*RecordingCapture) error) (RecordingCapture, error) {
	c, err := r.GetCapture(ctx, id)
	if err != nil {
		return c, err
	}
	c.Objects = append([]RecordingCaptureObject(nil), c.Objects...)
	receipts := map[string]RecordingCaptureStoredReceipt{}
	for k, v := range c.Receipts {
		receipts[k] = v
	}
	c.Receipts = receipts
	if err = fn(&c); err != nil {
		return c, err
	}
	r.captures[id] = c
	return c, nil
}
func captureTest(t *testing.T) (*RecordingCaptureService, *captureRepo, *packageObjects, *time.Time, RecordingCapture) {
	t.Helper()
	now := time.Now().UTC()
	repo := &captureRepo{captures: map[string]RecordingCapture{}}
	objects := &packageObjects{sizes: map[string]int64{}}
	s := NewRecordingCaptureService(repo, objects, func() time.Time { return now })
	r, err := s.Create(context.Background(), "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "create-a")
	if err != nil {
		t.Fatal(err)
	}
	return s, repo, objects, &now, repo.captures[r.Capture.ID]
}
func declareCapture(t *testing.T, s *RecordingCaptureService, c RecordingCapture, objects []RecordingPackageObject) {
	t.Helper()
	if _, err := s.Declare(context.Background(), c.ID, c.ActorID, "declare-a", objects); err != nil {
		t.Fatal(err)
	}
}
func TestCaptureDeclarationsOwnershipAndLimits(t *testing.T) {
	s, repo, _, _, c := captureTest(t)
	ctx := context.Background()
	o := RecordingPackageObject{Path: "720p/init.mp4", SizeBytes: 10, SHA256: strings.Repeat("a", 64)}
	declareCapture(t, s, c, []RecordingPackageObject{o})
	if _, err := s.Declare(ctx, c.ID, c.ActorID, "declare-b", []RecordingPackageObject{o}); err != nil {
		t.Fatal(err)
	}
	o.SizeBytes++
	if _, err := s.Declare(ctx, c.ID, c.ActorID, "declare-c", []RecordingPackageObject{o}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed declaration: %v", err)
	}
	if _, err := s.Sign(ctx, c.ID, "other", []string{o.Path}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("owner: %v", err)
	}
	if _, err := s.Sign(ctx, c.ID, c.ActorID, []string{"720p/seg-000000.m4s"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	o.Path = "720p/seg-000000.m4s"
	o.SizeBytes = RecordingObjectMaxBytes + 1
	if _, err := s.Declare(ctx, c.ID, c.ActorID, "oversize", []RecordingPackageObject{o}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	o.SizeBytes = 10
	for _, mode := range []string{"bytes", "count"} {
		p := repo.captures[c.ID]
		if mode == "bytes" {
			p.DeclaredBytes = RecordingPackageMaxBytes
		} else {
			p.DeclaredObjects = RecordingPackageMaxObjects
		}
		repo.captures[c.ID] = p
		if _, err := s.Declare(ctx, c.ID, c.ActorID, mode, []RecordingPackageObject{o}); !errors.Is(err, ErrRecordingPackageTooLarge) {
			t.Fatalf("%s: %v", mode, err)
		}
	}
}
func TestCaptureStopDoesNotFreezeUploads(t *testing.T) {
	s, _, objects, now, c := captureTest(t)
	o := RecordingPackageObject{Path: "720p/init.mp4", SizeBytes: 10, SHA256: strings.Repeat("a", 64)}
	declareCapture(t, s, c, []RecordingPackageObject{o})
	*now = now.Add(10 * time.Minute)
	if _, err := s.Sign(context.Background(), c.ID, c.ActorID, []string{o.Path}); err != nil {
		t.Fatal(err)
	}
	objects.sizes["recordings/packages/"+c.ID+"/staging/"+o.Path] = o.SizeBytes
	for i := 0; i < 2; i++ {
		r, err := s.Confirm(context.Background(), c.ID, c.ActorID, "confirm-a", []string{o.Path})
		if err != nil || r.Capture.Objects[0].State != "queued" || r.Capture.State != "uploading" {
			t.Fatalf("confirm receipt: %+v %v", r, err)
		}
	}
}
func TestSealMissingObjectsRemainsUploadable(t *testing.T) {
	s, _, objects, _, c := captureTest(t)
	inv := captureInventoryFixture()
	declareCapture(t, s, c, inv.Objects)
	if _, err := s.Seal(context.Background(), c.ID, c.ActorID, "seal-a", true, inv); !errors.Is(err, ErrCaptureMissingObjects) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := s.Sign(context.Background(), c.ID, c.ActorID, []string{inv.Objects[0].Path}); err != nil {
		t.Fatal(err)
	}
	paths := []string{}
	for _, o := range inv.Objects {
		paths = append(paths, o.Path)
		objects.sizes["recordings/packages/"+c.ID+"/staging/"+o.Path] = o.SizeBytes
	}
	if _, err := s.Confirm(context.Background(), c.ID, c.ActorID, "confirm-all", paths); err != nil {
		t.Fatal(err)
	}
	r, err := s.Seal(context.Background(), c.ID, c.ActorID, "seal-a", true, inv)
	if err != nil || r.Capture.State != "freezing" {
		t.Fatalf("seal: %+v %v", r, err)
	}
	replay, err := s.Seal(context.Background(), c.ID, c.ActorID, "seal-a", true, inv)
	if err != nil || !replay.Receipt.AcceptedAt.Equal(r.Receipt.AcceptedAt) {
		t.Fatal(err)
	}
	if _, err := s.Sign(context.Background(), c.ID, c.ActorID, paths[:1]); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err := s.Declare(context.Background(), c.ID, c.ActorID, "late", inv.Objects[:1]); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
}
func TestCaptureExpiryAbortAndReceiptConflict(t *testing.T) {
	s, repo, _, now, c := captureTest(t)
	ctx := context.Background()
	r, err := s.Abort(ctx, c.ID, c.ActorID, "abort-a", "user_abort")
	if err != nil || r.Capture.State != "aborted" {
		t.Fatal(err)
	}
	if _, err = s.Abort(ctx, c.ID, c.ActorID, "abort-a", "user_abort"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Abort(ctx, c.ID, c.ActorID, "abort-a", "encoder_failure"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	for _, state := range []string{"failed", "expired", "uploading"} {
		p := repo.captures[c.ID]
		p.State = state
		repo.captures[c.ID] = p
		*now = c.ExpiresAt
		if _, err = s.Sign(ctx, c.ID, c.ActorID, []string{"720p/init.mp4"}); err == nil {
			t.Fatalf("signed %s", state)
		}
	}
}

func TestCaptureCreateRequiresHumanAndRecordingUUID(t *testing.T) {
	s, _, _, _, _ := captureTest(t)
	for _, ids := range [][2]string{{"recording-a", "22222222-2222-4222-8222-222222222222"}, {"11111111-1111-4111-8111-111111111111", "service-principal"}} {
		if _, err := s.Create(context.Background(), ids[0], ids[1], "invalid-ids"); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("UUID boundary accepted: %v", err)
		}
	}
}

func captureInventoryFixture() RecordingPackageInventory {
	inv := packageFixture()
	inv.Renditions = []RecordingRendition{{Name: "1080p", Width: 1920, Height: 1080, FrameRate: 30, VideoBitrate: 3000000, AudioBitrate: 128000, DurationSeconds: 5, SegmentCount: 1}, {Name: "720p", Width: 1280, Height: 720, FrameRate: 30, VideoBitrate: 1500000, AudioBitrate: 128000, DurationSeconds: 5, SegmentCount: 1}, {Name: "480p", Width: 854, Height: 480, FrameRate: 30, VideoBitrate: 800000, AudioBitrate: 128000, DurationSeconds: 5, SegmentCount: 1}}
	inv.Objects = []RecordingPackageObject{{Path: "master.m3u8", SizeBytes: 10, SHA256: strings.Repeat("a", 64)}}
	for _, r := range inv.Renditions {
		for _, path := range []string{"index.m3u8", "init.mp4", "seg-000000.m4s"} {
			inv.Objects = append(inv.Objects, RecordingPackageObject{Path: r.Name + "/" + path, SizeBytes: 10, SHA256: strings.Repeat("b", 64)})
		}
	}
	inv.InventoryDigest, _ = RecordingInventoryDigest(inv)
	return inv
}

func TestCaptureReceiptLimitReservesAbortAfterSeal(t *testing.T) {
	ctx := context.Background()
	s, repo, objects, _, c := captureTest(t)
	inv := captureInventoryFixture()
	declareCapture(t, s, c, inv.Objects)
	paths := []string{}
	for _, o := range inv.Objects {
		paths = append(paths, o.Path)
		objects.sizes[(RecordingPackage{ID: c.ID}).StagingKey(o.Path)] = o.SizeBytes
	}
	if _, err := s.Confirm(ctx, c.ID, c.ActorID, "confirm-all", paths); err != nil {
		t.Fatal(err)
	}
	stored := repo.captures[c.ID]
	// Model create plus all 10,000 individual declare/confirm receipts before seal.
	for i := len(stored.Receipts); i < 20001; i++ {
		stored.Receipts[fmt.Sprintf("used-%d", i)] = RecordingCaptureStoredReceipt{Receipt: RecordingCaptureReceipt{Operation: "declare"}}
	}
	repo.captures[c.ID] = stored
	if _, err := s.Seal(ctx, c.ID, c.ActorID, "seal-max", true, inv); err != nil {
		t.Fatal(err)
	}
	if len(repo.captures[c.ID].Receipts) != 20002 {
		t.Fatal("seal did not fill normal receipt budget")
	}
	aborted, err := s.Abort(ctx, c.ID, c.ActorID, "emergency-abort", "user_abort")
	if err != nil || aborted.Capture.State != "aborted" {
		t.Fatalf("full sealed capture cannot abort: %v", err)
	}
	replay, err := s.Abort(ctx, c.ID, c.ActorID, "emergency-abort", "user_abort")
	if err != nil || !replay.Receipt.AcceptedAt.Equal(aborted.Receipt.AcceptedAt) || len(repo.captures[c.ID].Receipts) != 20003 {
		t.Fatalf("abort reserve/replay: %v", err)
	}
	if _, err = s.Abort(ctx, c.ID, c.ActorID, "emergency-abort", "encoder_failure"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed abort receipt accepted: %v", err)
	}
	if _, err = s.Abort(ctx, c.ID, c.ActorID, "second-abort", "user_abort"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second control receipt accepted: %v", err)
	}
	if len(repo.captures[c.ID].Receipts) != 20003 {
		t.Fatal("abort reserve exceeded total receipt cap")
	}
}
