package recordingvalidation

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
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
			go func() { _, err := copyLiveObjects(ctx, objects, claim, nil, &liveCopyTiming{}); done <- err }()
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
	if _, err := copyLiveObjects(context.Background(), objects, claim, nil, &liveCopyTiming{}); !errors.Is(err, assets.ErrInvalidUpload) || objects.copies.Load() != 0 {
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

func (s *publicationObjects) PublishLiveObject(ctx context.Context, from, to, etag string) (string, error) {
	if etag != "immutable-etag" {
		return "", errors.New("ETag fence missing")
	}
	n := s.copies.Add(1)
	s.active.Add(1)
	defer s.active.Add(-1)
	s.started <- struct{}{}
	if n == 1 {
		select {
		case <-s.fail:
			close(s.failed)
			return "", s.failure
		case <-s.release:
			return "published-etag", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	// A provider can finish late after cancellation; the caller must still join it.
	<-s.release
	return "published-etag", nil
}

type delayedPublicationHead struct {
	*publicationObjects
	calls   int
	delayed int
	err     error
}

func (s *delayedPublicationHead) Head(ctx context.Context, key string) (int64, string, error) {
	s.calls++
	if s.calls <= s.delayed {
		<-ctx.Done()
		return 0, "", ctx.Err()
	}
	if s.err != nil {
		return 0, "", s.err
	}
	return s.publicationObjects.Head(ctx, key)
}

func TestLivePublicationRetriesOnlyItsOwnHeadDeadline(t *testing.T) {
	denied := errors.New("forbidden")
	for _, tc := range []struct {
		name                    string
		delayed                 int
		stageBudget             time.Duration
		cancelAfter             time.Duration
		headErr, wantErr        error
		calls, copies, timeouts int
	}{
		{"fast", 0, 20 * time.Second, 0, nil, nil, 1, 1, 0},
		{"one stall", 1, 20 * time.Second, 0, nil, nil, 2, 1, 1},
		{"two stalls", 2, 20 * time.Second, 0, nil, context.DeadlineExceeded, 2, 0, 2},
		{"insufficient budget", 1, 5 * time.Second, 0, nil, context.DeadlineExceeded, 1, 0, 1},
		{"stage expired", 1, time.Second, 0, nil, context.DeadlineExceeded, 1, 0, 0},
		{"owner canceled", 1, 20 * time.Second, time.Second, nil, context.Canceled, 1, 0, 0},
		{"denied", 0, 20 * time.Second, 0, denied, denied, 1, 0, 0},
		{"provider deadline before local deadline", 0, 20 * time.Second, 0, context.DeadlineExceeded, context.DeadlineExceeded, 1, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), tc.stageBudget)
				defer cancel()
				if tc.cancelAfter > 0 {
					time.AfterFunc(tc.cancelAfter, cancel)
				}
				release := make(chan struct{})
				close(release)
				objects := &delayedPublicationHead{publicationObjects: &publicationObjects{started: make(chan struct{}, 1), release: release}, delayed: tc.delayed, err: tc.headErr}
				claim := postgres.RecordingLiveClaim{Objects: []assets.RecordingPackageObject{{Path: "fragment", SizeBytes: 10}}}
				var timing liveCopyTiming
				versions, err := copyLiveObjects(ctx, objects, claim, nil, &timing)
				if !errors.Is(err, tc.wantErr) || objects.calls != tc.calls || objects.copies.Load() != int64(tc.copies) || timing.headRetries != tc.calls-1 || timing.headTimeouts != tc.timeouts {
					t.Fatalf("err=%v calls=%d copies=%d retries=%d timeouts=%d", err, objects.calls, objects.copies.Load(), timing.headRetries, timing.headTimeouts)
				}
				if tc.copies > 0 && versions["fragment"] != "published-etag" {
					t.Fatal("missing conditional copy result", versions)
				}
			})
		})
	}
}
