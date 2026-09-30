package assets

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/storage/r2"
)

type packageRepo struct{ packages map[string]RecordingPackage }

func (r *packageRepo) WithSessionLock(ctx context.Context, _ string, fn func(context.Context) error) error {
	return fn(ctx)
}
func (r *packageRepo) FindByIdempotency(_ context.Context, key string) (RecordingPackage, error) {
	for _, p := range r.packages {
		if p.IdempotencyKey == key {
			return p, nil
		}
	}
	return RecordingPackage{}, ErrNotFound
}
func (r *packageRepo) Get(_ context.Context, id string) (RecordingPackage, error) {
	p, ok := r.packages[id]
	if !ok {
		return p, ErrNotFound
	}
	return p, nil
}
func (r *packageRepo) Create(_ context.Context, p RecordingPackage) error {
	r.packages[p.ID] = p
	return nil
}
func (r *packageRepo) Freeze(_ context.Context, id string, at time.Time) error {
	p := r.packages[id]
	if p.State != "uploading" || !at.Before(p.ExpiresAt) {
		return ErrConflict
	}
	p.State = "freezing"
	r.packages[id] = p
	return nil
}

type packageObjects struct {
	signed []string
	ttl    time.Duration
	sizes  map[string]int64
}

func (o *packageObjects) PresignPackageObject(_ context.Context, key string, _ int64, _ string, ttl time.Duration) (r2.PresignedPart, error) {
	o.signed = append(o.signed, key)
	o.ttl = ttl
	return r2.PresignedPart{URL: "https://object.invalid", Method: "PUT"}, nil
}
func (o *packageObjects) Head(_ context.Context, key string) (int64, string, error) {
	if size, ok := o.sizes[key]; ok {
		return size, "etag", nil
	}
	return 0, "", r2.ErrNotFound
}
func newPackageTest(t *testing.T) (*RecordingPackageService, *packageRepo, *packageObjects, *time.Time, CreateRecordingPackageInput) {
	t.Helper()
	now := time.Now().UTC()
	repo := &packageRepo{packages: map[string]RecordingPackage{}}
	objects := &packageObjects{sizes: map[string]int64{}}
	inv := packageFixture()
	inv.InventoryDigest, _ = RecordingInventoryDigest(inv)
	return NewRecordingPackageService(repo, objects, func() time.Time { return now }), repo, objects, &now,
		CreateRecordingPackageInput{ActorID: "admin-a", RecordingID: "recording-a", IdempotencyKey: "operation-a", Inventory: inv}
}

func TestRecordingPackageUploadOwnershipReplayAndFreeze(t *testing.T) {
	ctx := context.Background()
	svc, _, objects, now, input := newPackageTest(t)
	p, err := svc.Create(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := svc.Create(ctx, input)
	if err != nil || replayed.ID != p.ID {
		t.Fatalf("replay: %+v %v", replayed, err)
	}
	wrong := input
	wrong.ActorID = "another"
	if _, err := svc.Create(ctx, wrong); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-actor replay: %v", err)
	}
	wrong = input
	wrong.Inventory = packageFixture()
	wrong.Inventory.PresetVersion = "hls-v2"
	wrong.Inventory.InventoryDigest, _ = RecordingInventoryDigest(wrong.Inventory)
	if _, err := svc.Create(ctx, wrong); !errors.Is(err, ErrConflict) {
		t.Fatalf("different payload replay: %v", err)
	}
	if _, err := svc.Sign(ctx, p.ID, "another", []string{"master.m3u8"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ownership: %v", err)
	}
	if _, err := svc.Sign(ctx, p.ID, input.ActorID, []string{"master.m3u8", "../secret"}); !errors.Is(err, ErrInvalidInput) || len(objects.signed) != 0 {
		t.Fatalf("must validate entire batch before signing: %v", err)
	}
	if _, err := svc.Sign(ctx, p.ID, input.ActorID, []string{"master.m3u8", "master.m3u8"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := svc.Sign(ctx, p.ID, input.ActorID, make([]string, 101)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("batch cap: %v", err)
	}
	*now = p.ExpiresAt.Add(-time.Minute)
	if _, err := svc.Sign(ctx, p.ID, input.ActorID, []string{"master.m3u8"}); err != nil {
		t.Fatal(err)
	}
	if objects.ttl != time.Minute || !strings.HasSuffix(objects.signed[0], "/staging/master.m3u8") {
		t.Fatalf("scope: %v %v", objects.ttl, objects.signed)
	}
	p, err = svc.Complete(ctx, p.ID, input.ActorID)
	if err != nil || p.State != "freezing" {
		t.Fatalf("durable receipt: %+v %v", p, err)
	}
	if _, err := svc.Sign(ctx, p.ID, input.ActorID, []string{"master.m3u8"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("frozen package signed: %v", err)
	}
	*now = now.Add(2 * time.Hour)
	if replayed, err := svc.Complete(ctx, p.ID, input.ActorID); err != nil || replayed.State != "freezing" {
		t.Fatalf("accepted completion replay after upload expiry: %+v %v", replayed, err)
	}
}

func TestRecordingPackageStatusUsesRemoteEvidenceAndPages(t *testing.T) {
	ctx := context.Background()
	svc, _, objects, now, input := newPackageTest(t)
	p, err := svc.Create(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	objects.sizes[p.StagingKey("master.m3u8")] = 100
	objects.sizes[p.StagingKey("720p/index.m3u8")] = 99 // Size mismatch is not confirmed.
	page, err := svc.Status(ctx, p.ID, input.ActorID, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.ConfirmedObjects) != 0 || page.NextCursor != "720p/init.mp4" {
		t.Fatalf("first page: %+v", page)
	}
	page, err = svc.Status(ctx, p.ID, input.ActorID, page.NextCursor, 2)
	if err != nil || len(page.ConfirmedObjects) != 1 || page.ConfirmedObjects[0] != "master.m3u8" || page.NextCursor != "" {
		t.Fatalf("second page: %+v %v", page, err)
	}
	if _, err := svc.Status(ctx, p.ID, input.ActorID, "not-an-object", 2); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unknown cursor: %v", err)
	}
	if _, err := svc.Status(ctx, p.ID, input.ActorID, "", 1001); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("page cap: %v", err)
	}
	*now = p.ExpiresAt
	if _, err := svc.Sign(ctx, p.ID, input.ActorID, []string{"master.m3u8"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired signing: %v", err)
	}
	if _, err := svc.Complete(ctx, p.ID, input.ActorID); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired completion: %v", err)
	}
	*now = p.ExpiresAt.Add(-500 * time.Millisecond)
	if _, err := svc.Sign(ctx, p.ID, input.ActorID, []string{"master.m3u8"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("subsecond capability: %v", err)
	}
}
