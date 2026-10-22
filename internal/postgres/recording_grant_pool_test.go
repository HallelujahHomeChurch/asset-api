package postgres

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
	"strings"
	"testing"
	"time"
)

func TestBroadcastLiveGrantUsesOnePoolConnection(t *testing.T) {
	db := isolatedIntegrationDB(t)
	ctx := context.Background()
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	objects := &broadcastMemoryObjects{values: map[string][]byte{}}
	store := NewRecordingCaptureStore(db).WithBroadcastObjects(objects)
	now := time.Now().UTC().Truncate(time.Microsecond)
	c := assets.RecordingCapture{ID: strings.Repeat("a", 32), ActorID: "actor", RecordingID: "11111111-1111-4111-8111-111111111111", CreateKey: "grant-pool", State: "uploading", CreatedAt: now, ExpiresAt: now.Add(time.Hour), BroadcastEpoch: 1, Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}
	if _, err := store.CreateCapture(ctx, c); err != nil {
		t.Fatal(err)
	}
	policy := assets.RecordingBroadcastRange{RecordingID: c.RecordingID, Epoch: 1, RangeRevision: 2, StartSequence: boundary(0), MemberState: "live"}
	if _, err := store.SetBroadcastRange(ctx, c.ID, policy); err != nil {
		t.Fatal(err)
	}
	history := []assets.RecordingLiveSegment{}
	for i := 0; i < 3; i++ {
		history = append(history, assets.RecordingLiveSegment{Sequence: i, Renditions: map[string]assets.RecordingLiveFragment{"1080p": {Start: float64(i) * 10, End: float64(i+1) * 10}}})
	}
	raw, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `UPDATE recording_live SET segments=$2,revision=3,published_revision=3,published_sequence=2,published_media_end=30 WHERE capture_id=$1`, c.ID, raw); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := assets.NewRecordingSigner(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), "test-key", "grant-pool-test")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	grantCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	service := assets.NewRecordingCaptureService(store, captureObjectStore{}, time.Now)
	grant, err := service.GrantLive(grantCtx, signer, c.ID, c.RecordingID, "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333", now.Add(5*time.Minute))
	if err != nil || grant.ExchangeCredential == "" {
		t.Fatalf("live authorization must complete on its existing transaction connection: %v; pool waits=%d", err, db.Stats().WaitCount)
	}
	if db.Stats().WaitCount != 0 {
		t.Fatalf("grant borrowed a second connection: %+v", db.Stats())
	}
	var lease time.Time
	if err = db.QueryRowContext(ctx, `SELECT read_grant_until FROM recording_live WHERE capture_id=$1`, c.ID).Scan(&lease); err != nil || lease.Before(grant.ExpiresAt) {
		t.Fatalf("grant cleanup lease not committed: %v", err)
	}
	previewCtx, previewCancel := context.WithTimeout(ctx, time.Second)
	defer previewCancel()
	if _, err = service.GrantPreview(previewCtx, signer, c.ID, c.RecordingID, "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333", 1, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("preview must also use one connection: %v", err)
	}
	policy.RangeRevision++
	policy.Revoked = true
	policy.MemberState = "blocked"
	if _, err = store.SetBroadcastRange(ctx, c.ID, policy); err != nil {
		t.Fatal(err)
	}
	deniedCtx, deniedCancel := context.WithTimeout(ctx, time.Second)
	defer deniedCancel()
	if _, err = service.GrantLive(deniedCtx, signer, c.ID, c.RecordingID, "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333", now.Add(5*time.Minute)); !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("revoked authority must deny: %v", err)
	}
}
