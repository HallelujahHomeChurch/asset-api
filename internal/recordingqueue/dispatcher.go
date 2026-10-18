package recordingqueue

import (
	"context"
	"log/slog"
	"time"
)

type DispatchRepository interface {
	ClaimDispatch(context.Context) (Event, bool, error)
	Sent(context.Context, Event) error
	SendFailed(context.Context, Event) error
}
type Sender interface {
	Send(context.Context, Event) error
}
type Dispatcher struct {
	repo DispatchRepository
	send Sender
}

func NewDispatcher(r DispatchRepository, s Sender) *Dispatcher { return &Dispatcher{r, s} }
func (d *Dispatcher) Once(ctx context.Context) (bool, error) {
	event, ok, err := d.repo.ClaimDispatch(ctx)
	if err != nil || !ok {
		return false, err
	}
	if err = d.send.Send(ctx, event); err != nil {
		// A lost send response can leave a duplicate in Queue; the durable token fences it.
		_ = d.repo.SendFailed(ctx, event)
		return true, err
	}
	return true, d.repo.Sent(ctx, event)
}
func (d *Dispatcher) Run(ctx context.Context) error {
	for {
		poll, cancel := context.WithTimeout(ctx, 10*time.Second)
		worked, err := d.Once(poll)
		cancel()
		if err != nil {
			slog.Warn("recording_event_dispatch_failed")
		}
		if worked && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}
