package recordingqueue

import (
	"context"
	"errors"
	"testing"
)

type dispatchRepo struct {
	event        Event
	sent, failed bool
}

func (r *dispatchRepo) ClaimDispatch(context.Context) (Event, bool, error) { return r.event, true, nil }
func (r *dispatchRepo) Sent(context.Context, Event) error                  { r.sent = true; return nil }
func (r *dispatchRepo) SendFailed(context.Context, Event) error            { r.failed = true; return nil }

type senderFunc func(context.Context, Event) error

func (f senderFunc) Send(c context.Context, e Event) error { return f(c, e) }
func TestDispatchFailurePersistsRetryWithoutClaimingDelivered(t *testing.T) {
	r := &dispatchRepo{}
	d := NewDispatcher(r, senderFunc(func(context.Context, Event) error { return errors.New("queue unavailable") }))
	if _, err := d.Once(context.Background()); err == nil || !r.failed || r.sent {
		t.Fatal("send failure lost retry", err)
	}
}
