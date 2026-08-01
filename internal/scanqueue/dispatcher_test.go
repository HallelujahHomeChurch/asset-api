package scanqueue

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDispatchOnceMarksAcceptedMessageDelivered(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	repository := &memoryRepository{request: Request{EventID: "event-1", AssetID: "asset-1", ETag: "etag-1"}}
	sender := &memorySender{}
	dispatcher := NewDispatcher(repository, sender, func() time.Time { return now })

	dispatched, err := dispatcher.DispatchOnce(context.Background())
	if err != nil || !dispatched {
		t.Fatalf("dispatch: dispatched=%v err=%v", dispatched, err)
	}
	if len(sender.events) != 1 || sender.events[0].Version != 1 || sender.events[0].EventID != "event-1" {
		t.Fatalf("events=%+v", sender.events)
	}
	if repository.delivered != "event-1" {
		t.Fatalf("delivered=%q", repository.delivered)
	}
}

func TestDispatchOnceSchedulesSendFailureForRetry(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	repository := &memoryRepository{request: Request{EventID: "event-1", AssetID: "asset-1", ETag: "etag-1"}}
	sender := &memorySender{err: errors.New("queue unavailable")}
	dispatcher := NewDispatcher(repository, sender, func() time.Time { return now })

	dispatched, err := dispatcher.DispatchOnce(context.Background())
	if err != nil || !dispatched {
		t.Fatalf("dispatch: dispatched=%v err=%v", dispatched, err)
	}
	if !repository.nextAttempt.After(now) || repository.lastError == "" {
		t.Fatalf("nextAttempt=%v lastError=%q", repository.nextAttempt, repository.lastError)
	}
}

func TestDispatchOnceMayRedeliverAfterAcceptedMessageWasNotRecorded(t *testing.T) {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	repository := &memoryRepository{
		request:      Request{EventID: "event-1", AssetID: "asset-1", ETag: "etag-1"},
		markFailures: 1,
	}
	sender := &memorySender{}
	dispatcher := NewDispatcher(repository, sender, func() time.Time { return now })

	if _, err := dispatcher.DispatchOnce(context.Background()); err == nil {
		t.Fatal("first dispatch succeeded")
	}
	now = now.Add(time.Minute)
	if dispatched, err := dispatcher.DispatchOnce(context.Background()); err != nil || !dispatched {
		t.Fatalf("second dispatch: dispatched=%v err=%v", dispatched, err)
	}
	if len(sender.events) != 2 || sender.events[0].EventID != sender.events[1].EventID {
		t.Fatalf("events=%+v", sender.events)
	}
}

type memoryRepository struct {
	request      Request
	delivered    string
	lastError    string
	nextAttempt  time.Time
	claimedUntil time.Time
	attempts     int
	markFailures int
}

func (r *memoryRepository) ClaimScanRequest(_ context.Context, now time.Time, lease time.Duration) (Request, bool, error) {
	if r.delivered != "" || now.Before(r.nextAttempt) || now.Before(r.claimedUntil) {
		return Request{}, false, nil
	}
	r.attempts++
	r.request.Attempts = r.attempts
	r.claimedUntil = now.Add(lease)
	return r.request, true, nil
}

func (r *memoryRepository) MarkScanRequestDelivered(_ context.Context, eventID string, expectedAttempt int, _ time.Time) error {
	if r.markFailures > 0 {
		r.markFailures--
		return errors.New("database unavailable")
	}
	if expectedAttempt != r.attempts {
		return errors.New("stale attempt")
	}
	r.delivered = eventID
	return nil
}

func (r *memoryRepository) ScheduleScanRequestRetry(_ context.Context, _ string, expectedAttempt int, details string, nextAttempt, _ time.Time) error {
	if expectedAttempt != r.attempts {
		return errors.New("stale attempt")
	}
	r.lastError = details
	r.nextAttempt = nextAttempt
	r.claimedUntil = time.Time{}
	return nil
}

type memorySender struct {
	events []Event
	err    error
}

func (s *memorySender) Send(_ context.Context, event Event) error {
	s.events = append(s.events, event)
	return s.err
}
