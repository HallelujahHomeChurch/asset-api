package recordingqueue

import (
	"context"
	"encoding/json"
	"errors"
	"hhc/asset-api/internal/derivativequeue"
	"io"
	"log/slog"
	"strings"
	"time"
)

type Event struct {
	Type       string `json:"type"`
	Version    int    `json:"version"`
	Lane       string `json:"lane"`
	Generation int64  `json:"generation"`
	Token      string `json:"token"`
}

func Decode(raw string) (Event, error) {
	var e Event
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&e); err != nil {
		return e, err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return e, errors.New("trailing event payload")
	}
	if e.Type != "asset.recording.wake.v1" || e.Version != 1 || e.Generation < 1 || e.Token == "" || len(e.Token) > 128 || (e.Lane != "live" && e.Lane != "processing" && e.Lane != "preview") {
		return e, errors.New("invalid recording wake event")
	}
	return e, nil
}

type Repository interface {
	Begin(context.Context, Event) (bool, error)
	Renew(context.Context, Event) error
	Finish(context.Context, Event, bool) error
}
type Queue interface {
	Receive(context.Context, time.Duration) (derivativequeue.Message, bool, error)
	Renew(context.Context, derivativequeue.Message, time.Duration) (derivativequeue.Message, error)
	Ack(context.Context, derivativequeue.Message) error
	Poison(context.Context, derivativequeue.Message) error
}
type Job struct {
	repository Repository
	queue      Queue
	process    func(context.Context, string) error
	renewEvery time.Duration
	lane       string
}

func NewJob(r Repository, q Queue, lane string, p func(context.Context, string) error) *Job {
	return &Job{r, q, p, 30 * time.Second, lane}
}
func (j *Job) Run(ctx context.Context) (result error) {
	started := time.Now()
	claimed := false
	renewals := 0
	defer func() {
		slog.Info("recording_event_worker_exit", "lane", j.lane, "elapsed_ms", time.Since(started).Milliseconds(), "claims", boolCount(claimed), "renewals", renewals, "failed", result != nil)
	}()
	message, ok, err := j.queue.Receive(ctx, 2*time.Minute)
	if err != nil || !ok {
		return err
	}
	event, err := Decode(message.Body)
	if err != nil || event.Lane != j.lane {
		return j.queue.Poison(ctx, message)
	}
	active, err := j.repository.Begin(ctx, event)
	if err != nil {
		return err
	}
	if !active {
		return j.queue.Ack(ctx, message)
	}
	claimed = true
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- j.process(workCtx, event.Lane) }()
	timer := time.NewTicker(j.renewEvery)
	defer timer.Stop()
	for {
		select {
		case result := <-done:
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := j.repository.Finish(ctx, event, result == nil); err != nil {
				return err
			}
			if err := j.queue.Ack(ctx, message); err != nil {
				return err
			}
			return result
		case <-ctx.Done():
			cancel()
			<-done
			return ctx.Err()
		case <-timer.C:
			renewCtx, stop := context.WithTimeout(ctx, 10*time.Second)
			renewed, e := j.queue.Renew(renewCtx, message, 2*time.Minute)
			if e == nil {
				message = renewed
				renewals++
				e = j.repository.Renew(renewCtx, event)
			}
			stop()
			if e != nil {
				cancel()
				<-done
				return e
			}
		}
	}
}

func boolCount(v bool) int {
	if v {
		return 1
	}
	return 0
}
