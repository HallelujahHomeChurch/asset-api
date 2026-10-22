package postgres

import (
	"context"
	"errors"
	"hhc/asset-api/internal/assets"
	"testing"
	"time"
)

type pausedGrantAuthority struct {
	*broadcastMemoryObjects
	entered chan struct{}
	release chan struct{}
}

func (o *pausedGrantAuthority) ReadBroadcastRange(ctx context.Context, id string) ([]byte, error) {
	raw, err := o.broadcastMemoryObjects.ReadBroadcastRange(ctx, id)
	if err != nil {
		return nil, err
	}
	o.entered <- struct{}{}
	select {
	case <-o.release:
		return raw, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestGrantAuthorityReadDoesNotBlockCaptureControl(t *testing.T) {
	for _, change := range []string{"control", "revoke", "delete"} {
		t.Run(change, func(t *testing.T) {
			db, store, objects, c, policy := broadcastRevocationFixture(t)
			db.SetMaxOpenConns(1)
			paused := &pausedGrantAuthority{objects, make(chan struct{}, 2), make(chan struct{})}
			store.WithBroadcastObjects(paused)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := store.WithCaptureGrant(ctx, c.ID, func(current *assets.RecordingCapture, reader assets.RecordingBroadcastReader) error {
					if current.TerminalAt != nil || current.State == "expired" {
						return assets.ErrConflict
					}
					_, err := reader.GetBroadcastProjection(ctx, c.ID)
					return err
				})
				result <- err
			}()
			select {
			case <-paused.entered:
			case <-ctx.Done():
				t.Fatal("grant did not reach authority read")
			}
			controlCtx, stop := context.WithTimeout(ctx, 500*time.Millisecond)
			var err error
			switch change {
			case "control":
				_, err = store.UpdateCapture(controlCtx, c.ID, func(*assets.RecordingCapture) error { return nil })
			case "revoke":
				policy.RangeRevision++
				policy.Revoked, policy.MemberState = true, "blocked"
				_, err = store.SetBroadcastRange(controlCtx, c.ID, policy)
			case "delete":
				err = NewRecordingDeletionStore(db).DeleteRecording(controlCtx, c.RecordingID)
			}
			stop()
			close(paused.release)
			grantErr := <-result
			if err != nil {
				t.Fatalf("remote grant authority read blocked %s: %v", change, err)
			}
			if change == "control" && grantErr != nil {
				t.Fatal(grantErr)
			}
			if change != "control" && !errors.Is(grantErr, assets.ErrConflict) {
				t.Fatalf("stale authority must not survive %s: %v", change, grantErr)
			}
		})
	}
}

func TestIndependentGrantAuthorityReadsOverlap(t *testing.T) {
	db, store, objects, c, _ := broadcastRevocationFixture(t)
	db.SetMaxOpenConns(1)
	paused := &pausedGrantAuthority{objects, make(chan struct{}, 2), make(chan struct{})}
	store.WithBroadcastObjects(paused)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := store.WithCaptureGrant(ctx, c.ID, func(_ *assets.RecordingCapture, reader assets.RecordingBroadcastReader) error {
				_, err := reader.GetBroadcastProjection(ctx, c.ID)
				return err
			})
			results <- err
		}()
	}
	overlapped := true
	for range 2 {
		select {
		case <-paused.entered:
		case <-ctx.Done():
			overlapped = false
		}
	}
	close(paused.release)
	for range 2 {
		if err := <-results; err != nil {
			t.Errorf("grant failed: %v", err)
		}
	}
	if !overlapped {
		t.Fatal("independent grants serialized their remote authority reads")
	}
}
