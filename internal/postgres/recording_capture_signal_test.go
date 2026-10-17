package postgres

import (
	"context"
	"fmt"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
	"strings"
	"testing"
	"time"
)

func TestCaptureSignalPersistsAcrossRestartAndLateConfirm(t *testing.T) {
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if e := migrations.Run(ctx, db); e != nil {
		t.Fatal(e)
	}
	store := NewRecordingCaptureStore(db).WithBroadcastObjects(&broadcastMemoryObjects{values: map[string][]byte{}})
	now := time.Now().UTC().Truncate(time.Microsecond)
	received := now.Add(-2 * time.Minute)
	c := assets.RecordingCapture{ID: strings.Repeat("a", 32), ActorID: "22222222-2222-4222-8222-222222222222", RecordingID: "11111111-1111-4111-8111-111111111111", CreateKey: "signal", State: "uploading", CreatedAt: received, ExpiresAt: now.Add(time.Hour), BroadcastEpoch: 1, SegmentFrames: 300, Receipts: map[string]assets.RecordingCaptureStoredReceipt{}}
	if _, e := store.CreateCapture(ctx, c); e != nil {
		t.Fatal(e)
	}
	if _, e := store.UpdateCapture(ctx, c.ID, func(c *assets.RecordingCapture) error {
		c.LastMediaConfirmedAt = &received
		for n := 0; n < 3; n++ {
			for _, q := range []string{"480p", "720p", "1080p"} {
				c.Objects = append(c.Objects, assets.RecordingCaptureObject{RecordingPackageObject: assets.RecordingPackageObject{Path: fmt.Sprintf("%s/seg-%06d.m4s", q, n), SizeBytes: 10, SHA256: strings.Repeat("a", 64)}, State: "queued"})
			}
		}
		c.DeclaredObjects = 9
		c.DeclaredBytes = 90
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	svc := assets.NewRecordingCaptureService(store, nil, func() time.Time { return now })
	signal, e := svc.BroadcastSignal(ctx, c.ID, c.ActorID, true)
	if e != nil || signal.EndSequenceExclusive == nil || *signal.EndSequenceExclusive != 3 {
		t.Fatalf("%+v %v", signal, e)
	}
	restarted := NewRecordingCaptureStore(db)
	saved, e := restarted.GetCapture(ctx, c.ID)
	if e != nil || saved.IdleEndSequenceExclusive == nil || *saved.IdleEndSequenceExclusive != 3 || !saved.LastMediaConfirmedAt.Equal(received) || saved.State != "uploading" {
		t.Fatalf("lost durable signal %+v %v", saved, e)
	}
}
