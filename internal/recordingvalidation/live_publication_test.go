package recordingvalidation

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/postgres"
)

func TestLivePublicationBoundsCopiesAndJoinsFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			objects := &publicationObjects{started: make(chan struct{}, 6), release: make(chan struct{}), fail: make(chan struct{}), failed: make(chan struct{}), failure: errors.New("copy failed")}
			t.Cleanup(func() {
				select {
				case <-objects.release:
				default:
					close(objects.release)
				}
			})
			claim := postgres.RecordingLiveClaim{Capture: assets.RecordingCapture{ID: "capture"}, ClaimID: "claim"}
			for i := 0; i < 6; i++ {
				claim.Objects = append(claim.Objects, assets.RecordingPackageObject{Path: fmt.Sprint(i), SizeBytes: 10})
			}
			done := make(chan error, 1)
			go func() { done <- publishLiveObjects(ctx, objects, claim) }()
			for i := 0; i < 3; i++ {
				select {
				case <-objects.started:
				case <-ctx.Done():
					close(objects.release)
					t.Fatal("publication serialized provider copies")
				}
			}
			select {
			case <-objects.started:
				t.Fatal("publication exceeded three provider copies")
			default:
			}
			if fail {
				close(objects.fail)
				select {
				case <-objects.failed:
				case <-ctx.Done():
					t.Fatal("provider failure was not observed")
				}
				select {
				case err := <-done:
					t.Fatalf("returned before provider siblings joined: %v", err)
				case <-time.After(30 * time.Millisecond):
				}
			}
			close(objects.release)
			err := <-done
			if fail && !errors.Is(err, objects.failure) || !fail && err != nil {
				t.Fatalf("publication result: %v", err)
			}
			if objects.active.Load() != 0 || !fail && objects.copies.Load() != 6 {
				t.Fatal("publication returned with outstanding or missing copies")
			}
		})
	}
}

func TestLivePublicationRejectsChangedObjectSize(t *testing.T) {
	objects := &publicationObjects{}
	claim := postgres.RecordingLiveClaim{Objects: []assets.RecordingPackageObject{{Path: "fragment", SizeBytes: 11}}}
	if err := publishLiveObjects(context.Background(), objects, claim); !errors.Is(err, assets.ErrInvalidUpload) || objects.copies.Load() != 0 {
		t.Fatalf("changed object copied: %v", err)
	}
}

type publicationObjects struct {
	LiveObjects
	started chan struct{}
	release chan struct{}
	fail    chan struct{}
	failed  chan struct{}
	failure error
	active  atomic.Int64
	copies  atomic.Int64
}

func (s *publicationObjects) Head(context.Context, string) (int64, string, error) {
	return 10, "immutable-etag", nil
}

func (s *publicationObjects) PublishLiveObject(ctx context.Context, from, to, etag string) error {
	if etag != "immutable-etag" {
		return errors.New("ETag fence missing")
	}
	n := s.copies.Add(1)
	s.active.Add(1)
	defer s.active.Add(-1)
	s.started <- struct{}{}
	if n == 1 {
		select {
		case <-s.fail:
			close(s.failed)
			return s.failure
		case <-s.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// A provider can finish late after cancellation; the caller must still join it.
	<-s.release
	return nil
}
