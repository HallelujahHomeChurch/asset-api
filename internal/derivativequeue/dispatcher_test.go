package derivativequeue

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDispatchOnceMarksSuccessfulEventDelivered(t *testing.T) {
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	repository := &memoryRepository{request: Request{EventID: "event-1", AssetID: "asset-1", ETag: "etag-1"}}
	sender := &memorySender{}
	dispatcher := NewDispatcher(repository, sender, func() time.Time { return now })

	dispatched, err := dispatcher.DispatchOnce(context.Background())
	if err != nil || !dispatched {
		t.Fatalf("dispatch: dispatched=%v err=%v", dispatched, err)
	}
	if len(sender.events) != 1 || sender.events[0] != (Event{Type: "asset.derivative.requested.v1", Version: 1, EventID: "event-1", AssetID: "asset-1", ETag: "etag-1"}) {
		t.Fatalf("events=%+v", sender.events)
	}
	if repository.delivered != "event-1" {
		t.Fatalf("delivered=%q", repository.delivered)
	}
	if dispatched, err = dispatcher.DispatchOnce(context.Background()); err != nil || dispatched || len(sender.events) != 1 {
		t.Fatalf("redelivery: dispatched=%v events=%d err=%v", dispatched, len(sender.events), err)
	}
}

func TestDispatchOnceReschedulesFailureWithBoundedBackoff(t *testing.T) {
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	repository := &memoryRepository{request: Request{EventID: "event-1", AssetID: "asset-1", ETag: "etag-1"}, attempts: 99}
	sender := &memorySender{err: errors.New("queue unavailable")}
	dispatcher := NewDispatcher(repository, sender, func() time.Time { return now })

	dispatched, err := dispatcher.DispatchOnce(context.Background())
	if err != nil || !dispatched {
		t.Fatalf("dispatch: dispatched=%v err=%v", dispatched, err)
	}
	if repository.delivered != "" || repository.nextAttempt != now.Add(32*time.Second) || repository.lastError == "" {
		t.Fatalf("delivered=%q next=%v error=%q", repository.delivered, repository.nextAttempt, repository.lastError)
	}
}

func TestDispatcherStopsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dispatcher := NewDispatcher(&memoryRepository{}, &memorySender{}, time.Now)
	if err := dispatcher.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

type memoryRepository struct {
	request      Request
	delivered    string
	lastError    string
	nextAttempt  time.Time
	claimedUntil time.Time
	attempts     int
}

func (r *memoryRepository) ClaimDerivativeRequest(_ context.Context, now time.Time, lease time.Duration) (Request, bool, error) {
	if r.request.EventID == "" || r.delivered != "" || now.Before(r.nextAttempt) || now.Before(r.claimedUntil) {
		return Request{}, false, nil
	}
	r.attempts++
	r.request.Attempts = r.attempts
	r.claimedUntil = now.Add(lease)
	return r.request, true, nil
}

func (r *memoryRepository) MarkDerivativeRequestDelivered(_ context.Context, eventID string, _ int, _ time.Time) error {
	r.delivered = eventID
	return nil
}

func (r *memoryRepository) ScheduleDerivativeRequestRetry(_ context.Context, _ string, _ int, details string, nextAttempt, _ time.Time) error {
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
