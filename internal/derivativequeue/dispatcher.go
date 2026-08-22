package derivativequeue

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

const (
	dispatchLease = 30 * time.Second
	pollInterval  = time.Second
)

type Repository interface {
	ClaimDerivativeRequest(context.Context, time.Time, time.Duration) (Request, bool, error)
	MarkDerivativeRequestDelivered(context.Context, string, int, time.Time) error
	ScheduleDerivativeRequestRetry(context.Context, string, int, string, time.Time, time.Time) error
}

type Sender interface {
	Send(context.Context, Event) error
}

type Dispatcher struct {
	repository Repository
	sender     Sender
	now        func() time.Time
}

func NewDispatcher(repository Repository, sender Sender, now func() time.Time) *Dispatcher {
	return &Dispatcher{repository: repository, sender: sender, now: now}
}

func (d *Dispatcher) DispatchOnce(ctx context.Context) (bool, error) {
	now := d.now().UTC()
	request, ok, err := d.repository.ClaimDerivativeRequest(ctx, now, dispatchLease)
	if err != nil || !ok {
		return false, err
	}
	event := Event{Type: "asset.derivative.requested.v1", Version: 1, EventID: request.EventID, AssetID: request.AssetID, ETag: request.ETag}
	if err := d.sender.Send(ctx, event); err != nil {
		details := fmt.Sprintf("send derivative request: %.400s", err)
		return true, d.repository.ScheduleDerivativeRequestRetry(ctx, request.EventID, request.Attempts, details, now.Add(retryDelay(request.Attempts)), now)
	}
	return true, d.repository.MarkDerivativeRequestDelivered(ctx, request.EventID, request.Attempts, now)
}

func (d *Dispatcher) Run(ctx context.Context) error {
	for {
		dispatched, err := d.DispatchOnce(ctx)
		if err != nil {
			slog.Warn("derivative outbox dispatch failed", "error", err)
		}
		delay := pollInterval
		if dispatched && err == nil {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Second * time.Duration(1<<(attempt-1))
}
