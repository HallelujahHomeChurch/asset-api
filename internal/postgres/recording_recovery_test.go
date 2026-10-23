package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
)

func recoveryPGFixture(t *testing.T) (*sql.DB, *RecordingCaptureStore, assets.RecordingCapture, captureObjectStore, assets.RecordingBroadcastRange) {
	t.Helper()
	ctx := context.Background()
	db := isolatedIntegrationDB(t)
	if err := migrations.Run(ctx, db); err != nil {
		t.Fatal(err)
	}
	store := NewRecordingCaptureStore(db).WithBroadcastObjects(&broadcastMemoryObjects{values: map[string][]byte{}})
	objects := captureObjectStore{map[string]int64{}}
	service := assets.NewRecordingCaptureService(store, objects, time.Now)
	created, err := service.CreateBroadcastWithRecovery(ctx, "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "create", 1, 300, assets.RecordingRecoveryVersion)
	if err != nil {
		t.Fatal(err)
	}
	id := created.Capture.ID
	actor := "22222222-2222-4222-8222-222222222222"
	var declared []assets.RecordingPackageObject
	var initial []string
	for n := 0; n < 2; n++ {
		for _, q := range []string{"480p", "720p", "1080p"} {
			path := fmt.Sprintf("%s/seg-%06d.m4s", q, n)
			declared = append(declared, assets.RecordingPackageObject{Path: path, SizeBytes: 10, SHA256: strings.Repeat("a", 64)})
			objects.sizes[path] = 10
			if n == 0 {
				initial = append(initial, path)
			}
		}
	}
	if _, err = service.Declare(ctx, id, actor, "declare", declared); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Confirm(ctx, id, actor, "initial", initial); err != nil {
		t.Fatal(err)
	}
	history := []assets.RecordingLiveSegment{{Sequence: 0, Renditions: map[string]assets.RecordingLiveFragment{"1080p": {FrameRate: 30000.0 / 1001, VideoFrames: 300, Start: 0, End: 10.01}}}}
	raw, _ := json.Marshal(history)
	if _, err = db.ExecContext(ctx, `UPDATE recording_live SET segments=$2 WHERE capture_id=$1`, id, raw); err != nil {
		t.Fatal(err)
	}
	start := 0
	policy := assets.RecordingBroadcastRange{RecordingID: created.Capture.RecordingID, Epoch: 1, RangeRevision: 2, StartSequence: &start, MemberState: "live"}
	if _, err = store.SetBroadcastRange(ctx, id, policy); err != nil {
		t.Fatal(err)
	}
	c, err := store.GetCapture(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Recovery.Active || c.Recovery.Deadline == nil || c.Recovery.CommonEnd != 1 || c.Recovery.SegmentDuration != 10010*time.Millisecond {
		t.Fatalf("activation %+v", c.Recovery)
	}
	return db, store, c, objects, policy
}

func TestRecoveryPersistsAcrossReplicasAndDeadlineRace(t *testing.T) {
	for _, delay := range []time.Duration{299 * time.Second, 300 * time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			db, store, c, objects, _ := recoveryPGFixture(t)
			db.SetMaxOpenConns(2)
			now := c.Recovery.ActivationAt.Add(delay)
			first := assets.NewRecordingCaptureService(store, objects, func() time.Time { return now })
			second := assets.NewRecordingCaptureService(NewRecordingCaptureStore(db), objects, func() time.Time { return now })
			ready := make(chan struct{})
			var wg sync.WaitGroup
			errs := make(chan error, 2)
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-ready
				_, err := first.Confirm(context.Background(), c.ID, c.ActorID, "second", []string{"480p/seg-000001.m4s", "720p/seg-000001.m4s", "1080p/seg-000001.m4s"})
				errs <- err
			}()
			go func() {
				defer wg.Done()
				<-ready
				_, err := second.BroadcastSignal(context.Background(), c.ID, c.ActorID, true)
				errs <- err
			}()
			close(ready)
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			persisted, err := NewRecordingCaptureStore(db).GetCapture(context.Background(), c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if delay < 300*time.Second {
				if persisted.IdleEndSequenceExclusive != nil || persisted.Recovery.CommonEnd != 2 || !persisted.Recovery.Deadline.Equal(c.Recovery.Deadline.Add(10010*time.Millisecond)) {
					t.Fatalf("before deadline %+v", persisted.Recovery)
				}
			} else {
				if persisted.IdleEndSequenceExclusive == nil || *persisted.IdleEndSequenceExclusive != 1 || persisted.Recovery.CommonEnd != 1 || !persisted.Recovery.Deadline.Equal(*c.Recovery.Deadline) {
					t.Fatalf("late confirm reopened %+v", persisted.Recovery)
				}
				if persisted.Objects[1].State == "declared" {
					t.Fatal("late supplementation was rejected")
				}
			}
		})
	}
}
func TestRecoveryRangeReplayAndTerminalFences(t *testing.T) {
	for _, change := range []string{"replay", "revoke", "stop", "delete"} {
		t.Run(change, func(t *testing.T) {
			db, store, c, objects, policy := recoveryPGFixture(t)
			policy.RangeRevision++
			switch change {
			case "revoke":
				policy.Revoked = true
				policy.MemberState = "blocked"
			case "stop":
				end := 1
				policy.EndSequenceExclusive = &end
				policy.MemberState = "vod"
			}
			var err error
			if change == "delete" {
				err = NewRecordingDeletionStore(db).DeleteRecording(context.Background(), c.RecordingID)
			} else {
				_, err = store.SetBroadcastRange(context.Background(), c.ID, policy)
			}
			if err != nil {
				t.Fatal(err)
			}
			current, err := store.GetCapture(context.Background(), c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !current.Recovery.ActivationAt.Equal(*c.Recovery.ActivationAt) || !current.Recovery.Deadline.Equal(*c.Recovery.Deadline) {
				t.Fatal("authority reset")
			}
			if change != "replay" && current.Recovery.Active {
				t.Fatal("terminal range retained recovery")
			}
			svc := assets.NewRecordingCaptureService(NewRecordingCaptureStore(db), objects, func() time.Time { return c.Recovery.ActivationAt.Add(time.Minute) })
			if _, err = svc.Confirm(context.Background(), c.ID, c.ActorID, "next", []string{"480p/seg-000001.m4s", "720p/seg-000001.m4s", "1080p/seg-000001.m4s"}); change == "delete" && err == nil {
				t.Fatal("deleted capture accepted confirmation")
			}
			current, err = store.GetCapture(context.Background(), c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if change != "replay" && !current.Recovery.Deadline.Equal(*c.Recovery.Deadline) {
				t.Fatal("terminal authority renewed")
			}
		})
	}
}
