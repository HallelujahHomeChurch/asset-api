package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
	"strings"
	"sync"
	"testing"
	"time"
)

type broadcastMemoryObjects struct {
	mu     sync.Mutex
	values map[string][]byte
	fail   bool
}

func (o *broadcastMemoryObjects) PutBroadcastRange(_ context.Context, id string, data []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fail {
		return errors.New("storage unavailable")
	}
	var next assets.RecordingBroadcastRange
	if err := json.Unmarshal(data, &next); err != nil {
		return err
	}
	if raw := o.values[id]; raw != nil {
		var old assets.RecordingBroadcastRange
		json.Unmarshal(raw, &old)
		if err := assets.ValidateBroadcastRangeChange(&old, next); err != nil {
			return err
		}
	}
	o.values[id] = append([]byte{}, data...)
	return nil
}
func (o *broadcastMemoryObjects) ReadBroadcastRange(_ context.Context, id string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fail {
		return nil, errors.New("storage unavailable")
	}
	return append([]byte{}, o.values[id]...), nil
}
func boundary(n int) *int { return &n }
func TestBroadcastCreateDenyAllAndDurableCAS(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	objects := &broadcastMemoryObjects{values: map[string][]byte{}}
	store := NewRecordingCaptureStore(db).WithBroadcastObjects(objects)
	now := time.Now()
	c := assets.RecordingCapture{ID: strings.Repeat("d", 32), ActorID: "actor", RecordingID: "11111111-1111-4111-8111-111111111111", CreateKey: "b1-create", State: "uploading", CreatedAt: now, ExpiresAt: now.Add(time.Hour), BroadcastEpoch: 1, Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}
	if _, err := store.CreateCapture(ctx, c); err != nil {
		t.Fatal(err)
	}
	p, err := store.GetBroadcastRange(ctx, c.ID)
	if err != nil || p == nil || p.StartSequence != nil || p.RangeRevision != 1 || p.MemberState != "blocked" {
		t.Fatalf("atomic deny all: %+v %v", p, err)
	}
	first := *p
	first.RangeRevision = 2
	first.StartSequence = boundary(8)
	first.MemberState = "live"
	if _, err := store.SetBroadcastRange(ctx, c.ID, first); err != nil {
		t.Fatal(err)
	}
	final := first
	final.RangeRevision = 3
	final.EndSequenceExclusive = boundary(20)
	final.MemberState = "vod"
	if _, err := store.SetBroadcastRange(ctx, c.ID, final); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetBroadcastRange(ctx, c.ID, final); err != nil {
		t.Fatalf("idempotent: %v", err)
	}
	stale := final
	stale.Epoch = 2
	stale.RangeRevision = 4
	if _, err := store.SetBroadcastRange(ctx, c.ID, stale); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("stale binding: %v", err)
	}
	expanded := final
	expanded.RangeRevision++
	expanded.EndSequenceExclusive = boundary(21)
	if _, err := store.SetBroadcastRange(ctx, c.ID, expanded); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("frozen end: %v", err)
	}
	var history []assets.RecordingLiveSegment
	for i := 0; i < 24; i++ {
		batch := map[string]assets.RecordingLiveFragment{}
		for _, r := range assets.LiveRenditions() {
			batch[r.Name] = assets.RecordingLiveFragment{FrameRate: 30000.0 / 1001, Start: float64(i) * 30.03, End: float64(i+1) * 30.03, Codecs: "avc1.64001f,mp4a.40.2"}
		}
		history = append(history, assets.RecordingLiveSegment{Sequence: i, Renditions: batch})
	}
	raw, _ := json.Marshal(history)
	if _, err := db.ExecContext(ctx, `UPDATE recording_live SET segments=$2,revision=25,published_revision=25,published_sequence=23,ended=true WHERE capture_id=$1`, c.ID, raw); err != nil {
		t.Fatal(err)
	}
	projection, err := store.GetBroadcastProjection(ctx, c.ID)
	if err != nil || projection.State != "ready" || projection.DurationSeconds == nil || *projection.DurationSeconds < 360.359 || *projection.DurationSeconds > 360.361 {
		t.Fatalf("projection: %+v %v", projection, err)
	}
	revoke := final
	revoke.RangeRevision = 4
	revoke.Revoked = true
	objects.fail = true
	if _, err := store.SetBroadcastRange(ctx, c.ID, revoke); err == nil {
		t.Fatal("provider failure acknowledged")
	}
	p, err = store.GetBroadcastRange(ctx, c.ID)
	if err != nil || p.Revoked || p.RangeRevision != 3 {
		t.Fatalf("unacknowledged state persisted: %+v %v", p, err)
	}
	if _, err := store.GetBroadcastProjection(ctx, c.ID); err == nil {
		t.Fatal("authority unavailable read ready")
	}
	objects.fail = false
	if _, err := store.SetBroadcastRange(ctx, c.ID, revoke); err != nil {
		t.Fatal(err)
	}
	p, err = NewRecordingCaptureStore(db).WithBroadcastObjects(objects).GetBroadcastRange(ctx, c.ID)
	if err != nil || !p.Revoked {
		t.Fatalf("restart lost revocation: %+v %v", p, err)
	}
}
func TestBroadcastRebindRequiresTerminalUnpublishedCapture(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	objects := &broadcastMemoryObjects{values: map[string][]byte{}}
	store := NewRecordingCaptureStore(db).WithBroadcastObjects(objects)
	now := time.Now()
	c := assets.RecordingCapture{ID: strings.Repeat("e", 32), ActorID: "actor", RecordingID: "11111111-1111-4111-8111-111111111111", CreateKey: "epoch-one", State: "uploading", CreatedAt: now, ExpiresAt: now.Add(time.Hour), BroadcastEpoch: 1, Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}
	if _, err := store.CreateCapture(ctx, c); err != nil {
		t.Fatal(err)
	}
	second := c
	second.ID = strings.Repeat("f", 32)
	second.CreateKey = "epoch-two"
	second.BroadcastEpoch = 2
	if _, err := store.CreateCapture(ctx, second); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("active rebound: %v", err)
	}
	if _, err := store.UpdateCapture(ctx, c.ID, func(c *assets.RecordingCapture) error { c.State = "aborted"; c.TerminalAt = &now; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateCapture(ctx, second); err != nil {
		t.Fatalf("safe epoch rebind: %v", err)
	}
	p, err := store.GetBroadcastRange(ctx, second.ID)
	if err != nil || p.Epoch != 2 || p.StartSequence != nil {
		t.Fatalf("new epoch exposure: %+v %v", p, err)
	}
	legacy := second
	legacy.ID = strings.Repeat("1", 32)
	legacy.RecordingID = "legacy-recording"
	legacy.CreateKey = "legacy-create"
	legacy.BroadcastEpoch = 0
	if _, err := store.CreateCapture(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if p, err := store.GetBroadcastRange(ctx, legacy.ID); err != nil || p != nil {
		t.Fatalf("legacy policy changed: %+v %v", p, err)
	}
}

func TestBroadcastImagesUsePublicStartAndPreCaptureCustomScopes(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	recording := "11111111-1111-4111-8111-111111111111"
	covers := NewRecordingLiveCoverStore(db)
	custom, err := covers.Create(ctx, "broadcast-"+recording, recording, "actor", "custom", "image/png", "digest", "custom")
	if err != nil {
		t.Fatal(err)
	}
	if err = covers.Queue(ctx, custom.ID); err != nil {
		t.Fatal(err)
	}
	job, err := covers.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = covers.Finish(ctx, job, "normalized-digest", true); err != nil {
		t.Fatal(err)
	}
	if err = covers.Retain(ctx, custom.ID, "broadcast-"+recording, recording, "broadcast:"+recording); err != nil {
		t.Fatal(err)
	}
	if _, err = covers.Get(ctx, custom.ID, "broadcast-"+recording, "22222222-2222-4222-8222-222222222222"); !errors.Is(err, assets.ErrInvalidInput) {
		t.Fatalf("cover owner leak: %v", err)
	}
	objects := &broadcastMemoryObjects{values: map[string][]byte{}}
	store := NewRecordingCaptureStore(db).WithBroadcastObjects(objects)
	now := time.Now()
	c := assets.RecordingCapture{ID: strings.Repeat("b", 32), ActorID: "actor", RecordingID: recording, CreateKey: "images", State: "uploading", CreatedAt: now, ExpiresAt: now.Add(time.Hour), BroadcastEpoch: 1, Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}
	if _, err = store.CreateCapture(ctx, c); err != nil {
		t.Fatal(err)
	}
	p := assets.RecordingBroadcastRange{MemberState: "blocked", RecordingID: recording, Epoch: 1, RangeRevision: 2, StartSequence: boundary(8), EndSequenceExclusive: boundary(20)}
	if _, err = store.SetBroadcastRange(ctx, c.ID, p); err != nil {
		t.Fatal(err)
	}
	var history []assets.RecordingLiveSegment
	for i := 0; i < 24; i++ {
		batch := map[string]assets.RecordingLiveFragment{}
		for _, r := range assets.LiveRenditions() {
			batch[r.Name] = assets.RecordingLiveFragment{FrameRate: 30000.0 / 1001, Start: float64(i) * 30.03, End: float64(i+1) * 30.03, Codecs: "avc1.64001f,mp4a.40.2"}
		}
		history = append(history, assets.RecordingLiveSegment{Sequence: i, Renditions: batch})
	}
	raw, _ := json.Marshal(history)
	if _, err = db.ExecContext(ctx, `UPDATE recording_live SET segments=$2,revision=24,published_revision=24,published_sequence=23 WHERE capture_id=$1`, c.ID, raw); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"480p/init.mp4", "480p/seg-000008.m4s"} {
		if _, err = db.ExecContext(ctx, `INSERT INTO recording_capture_objects(capture_id,path,size_bytes,sha256,state) VALUES($1,$2,10,$3,'verified')`, c.ID, name, strings.Repeat("c", 64)); err != nil {
			t.Fatal(err)
		}
	}
	automatic, err := covers.Create(ctx, c.ID, recording, "system", "auto", "", "", "auto")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := covers.Claim(ctx)
	if err != nil || claim.ID != automatic.ID {
		t.Fatalf("automatic claim: %+v %v", claim, err)
	}
	duration, media, err := covers.AutoMedia(ctx, claim)
	if err != nil || duration < 30.029 || duration > 30.031 || len(media) != 2 || media[1].Path != "480p/seg-000008.m4s" {
		t.Fatalf("prestart auto cover: %v %+v %v", duration, media, err)
	}
	timeline, err := covers.Timeline(ctx, c.ID)
	if err != nil || len(timeline) != 12 || timeline[0].Sequence != 8 || timeline[11].Sequence != 19 {
		t.Fatalf("public cover timeline: %+v %v", timeline, err)
	}
	p.RangeRevision++
	p.Revoked = true
	if _, err = store.SetBroadcastRange(ctx, c.ID, p); err != nil {
		t.Fatal(err)
	}
	if _, _, err = covers.AutoMedia(ctx, claim); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("revoked automatic cover: %v", err)
	}
}

func TestBroadcastCoverPromotionPreservesSourceOwnershipAndReplay(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	capture, recording := strings.Repeat("e", 32), "11111111-1111-4111-8111-111111111111"
	pkg := testRecordingPackage(t, capture, "actor", recording)
	if err := NewRecordingPackageStore(db).Create(ctx, pkg); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE recording_packages SET state='ready',completed_at=now(),ready_at=now(),final_prefix='recordings/packages/promotion/final/fixture/',media_expires_at=now()+interval '30 days' WHERE id=$1`, capture); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingLiveCoverStore(db)
	for _, scope := range []string{"defaults", "broadcast-" + recording} {
		owner := recording
		if scope == "defaults" {
			owner = ""
		}
		upload, err := store.Create(ctx, scope, owner, "actor", scope, "image/png", "input", "custom")
		if err != nil {
			t.Fatal(err)
		}
		if err = store.Queue(ctx, upload.ID); err != nil {
			t.Fatal(err)
		}
		claim, err := store.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.Finish(ctx, claim, "normalized", true); err != nil {
			t.Fatal(err)
		}
		cover, err := store.PromoteTo(ctx, upload.ID, scope, owner, capture, recording)
		if err != nil {
			t.Fatal(err)
		}
		replay, err := store.PromoteTo(ctx, upload.ID, scope, owner, capture, recording)
		if err != nil || replay.ID != cover.ID {
			t.Fatalf("promotion replay %+v %v", replay, err)
		}
		if cover.InheritedCoverID != upload.ID || cover.PackageID != capture {
			t.Fatalf("lost source %+v", cover)
		}
		if _, err = store.PromoteTo(ctx, upload.ID, scope, owner, capture, "22222222-2222-4222-8222-222222222222"); err == nil {
			t.Fatal("cross recording promotion")
		}
	}
}
