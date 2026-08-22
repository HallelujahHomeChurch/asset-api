package derivativequeue

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

const messageVisibility = 3*time.Minute + 30*time.Second

type Processor interface {
	ProcessAsset(context.Context, string, string) error
}

type MessageQueue interface {
	Receive(context.Context, time.Duration) (Message, bool, error)
	Ack(context.Context, Message) error
	Retry(context.Context, Message, time.Duration) error
	ForwardPoison(context.Context, Message, *Event, string, time.Time) error
}

type Job struct {
	processor   Processor
	queue       MessageQueue
	maxAttempts int64
	now         func() time.Time
}

func NewJob(processor Processor, queue MessageQueue, maxAttempts int) *Job {
	if maxAttempts < 1 {
		maxAttempts = 5
	}
	return &Job{processor: processor, queue: queue, maxAttempts: int64(maxAttempts), now: time.Now}
}

func (j *Job) RunOnce(ctx context.Context) (bool, error) {
	message, ok, err := j.queue.Receive(ctx, messageVisibility)
	if err != nil || !ok {
		return false, err
	}
	event, err := decodeEvent(message.Body)
	if err != nil {
		return true, j.poison(ctx, message, nil, "invalid_payload")
	}
	if err := j.processor.ProcessAsset(ctx, event.AssetID, event.ETag); err == nil {
		return true, j.queue.Ack(ctx, message)
	}
	if message.DequeueCount >= j.maxAttempts {
		return true, j.poison(ctx, message, &event, "processing_failed")
	}
	return true, j.queue.Retry(ctx, message, retryDelay(int(message.DequeueCount)))
}

func (j *Job) poison(ctx context.Context, message Message, event *Event, reason string) error {
	if err := j.queue.ForwardPoison(ctx, message, event, reason, j.now().UTC()); err != nil {
		return err
	}
	return j.queue.Ack(ctx, message)
}

func decodeEvent(body string) (Event, error) {
	var event Event
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil {
		return Event{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Event{}, errors.New("derivative event has trailing content")
	}
	if event.Type != "asset.derivative.requested.v1" || event.Version != 1 || event.EventID == "" || event.AssetID == "" || event.ETag == "" || len(event.EventID) > 200 || len(event.AssetID) > 200 || len(event.ETag) > 200 {
		return Event{}, errors.New("invalid derivative event")
	}
	return event, nil
}
