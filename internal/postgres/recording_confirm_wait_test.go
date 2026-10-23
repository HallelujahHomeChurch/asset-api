package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

type pausedConfirmObjects struct {
	captureObjectStore
	entered chan struct{}
	release chan struct{}
}

func (o *pausedConfirmObjects) ListPackageObjects(ctx context.Context, _ string, _ int) (map[string]int64, error) {
	o.entered <- struct{}{}
	select {
	case <-o.release:
		return o.sizes, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestConfirmStorageWaitDoesNotHoldDatabaseCapacity(t *testing.T) {
	for _, poolSize := range []int{1, 2} {
		for _, change := range []string{"control", "delete", "epoch", "cancel"} {
			t.Run(fmt.Sprintf("pool%d/%s", poolSize, change), func(t *testing.T) {
				db, store, _, c, _ := broadcastRevocationFixture(t)
				db.SetMaxOpenConns(poolSize)
				object := assets.RecordingPackageObject{Path: "720p/seg-000000.m4s", SizeBytes: 10, SHA256: strings.Repeat("a", 64)}
				paused := &pausedConfirmObjects{captureObjectStore{map[string]int64{object.Path: object.SizeBytes}}, make(chan struct{}, 1), make(chan struct{})}
				service := assets.NewRecordingCaptureService(store, paused, time.Now)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := service.Declare(ctx, c.ID, c.ActorID, "declare", []assets.RecordingPackageObject{object}); err != nil {
					t.Fatal(err)
				}
				result := make(chan error, 1)
				go func() {
					_, err := service.Confirm(ctx, c.ID, c.ActorID, "confirm", []string{object.Path})
					result <- err
				}()
				select {
				case <-paused.entered:
				case <-ctx.Done():
					t.Fatal("LIST did not start")
				}
				controlCtx, stop := context.WithTimeout(context.Background(), 500*time.Millisecond)
				err := db.PingContext(controlCtx)
				if err == nil {
					switch change {
					case "delete":
						err = NewRecordingDeletionStore(db).DeleteRecording(controlCtx, c.RecordingID)
					case "epoch":
						_, err = db.ExecContext(controlCtx, `UPDATE recording_captures SET broadcast_epoch=broadcast_epoch+1 WHERE id=$1`, c.ID)
					default:
						_, err = store.GetCapture(controlCtx, c.ID)
					}
				}
				stop()
				var confirmErr error
				if change == "cancel" {
					cancel()
					select {
					case confirmErr = <-result:
					case <-time.After(time.Second):
						t.Fatal("canceled storage did not release confirm")
					}
					close(paused.release)
				} else {
					close(paused.release)
					confirmErr = <-result
				}
				if err != nil {
					t.Fatalf("storage LIST blocked database/control: %v", err)
				}
				if change == "control" && confirmErr != nil {
					t.Fatal(confirmErr)
				}
				if change == "delete" && !errors.Is(confirmErr, assets.ErrCaptureExpired) {
					t.Fatalf("delete fence: %v", confirmErr)
				}
				if change == "epoch" && !errors.Is(confirmErr, assets.ErrConflict) {
					t.Fatalf("epoch fence: %v", confirmErr)
				}
				if change == "cancel" && !errors.Is(confirmErr, context.Canceled) {
					t.Fatalf("cancellation: %v", confirmErr)
				}
				current, err := store.GetCapture(context.Background(), c.ID)
				if err != nil {
					t.Fatal(err)
				}
				if change == "control" {
					if current.LastMediaConfirmedAt == nil {
						t.Fatal("missing watermark")
					}
				} else if current.LastMediaConfirmedAt != nil {
					t.Fatal("rejected confirmation advanced watermark")
				}
			})
		}
	}
}
