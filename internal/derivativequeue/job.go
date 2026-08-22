package derivativequeue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/derivatives"
)

const messageVisibility = 3*time.Minute + 30*time.Second

type Processor interface {
	ProcessAsset(context.Context, string, string) (derivatives.ProcessResult, error)
}

type PoisonRepository interface {
	RecordDerivativePoison(context.Context, assets.DerivativePoison, time.Time) (bool, error)
	FailDerivativeToPoison(context.Context, assets.ProcessingFailure, assets.DerivativePoison, time.Time) (bool, error)
	MarkDerivativePoisonForwarded(context.Context, string, time.Time) error
}

type MessageQueue interface {
	Receive(context.Context, time.Duration) (Message, bool, error)
	Ack(context.Context, Message) error
	Retry(context.Context, Message, time.Duration) error
	ForwardPoison(context.Context, Message, *Event, string, time.Time) error
}

type Job struct {
	processor  Processor
	repository PoisonRepository
	queue      MessageQueue
	now        func() time.Time
}

func NewJob(processor Processor, repository PoisonRepository, queue MessageQueue) *Job {
	return &Job{processor: processor, repository: repository, queue: queue, now: time.Now}
}

func (j *Job) RunOnce(ctx context.Context) (bool, error) {
	message, ok, err := j.queue.Receive(ctx, messageVisibility)
	if err != nil || !ok {
		return false, err
	}
	event, err := decodeEvent(message.Body)
	if err != nil {
		return true, j.poison(ctx, message, nil, "invalid_payload", err.Error(), derivatives.ProcessResult{})
	}
	result, err := j.processor.ProcessAsset(ctx, event.AssetID, event.ETag)
	if err != nil {
		return true, j.queue.Retry(ctx, message, 15*time.Second)
	}
	switch result.State {
	case derivatives.ProcessSatisfied:
		return true, j.queue.Ack(ctx, message)
	case derivatives.ProcessRetry:
		delay := result.RetryAt.Sub(j.now().UTC())
		if delay < time.Second {
			delay = time.Second
		}
		return true, j.queue.Retry(ctx, message, delay)
	case derivatives.ProcessTerminal:
		return true, j.poison(ctx, message, &event, "processing_failed", result.Details, result)
	default:
		return true, j.queue.Retry(ctx, message, 15*time.Second)
	}
}

func (j *Job) poison(ctx context.Context, message Message, event *Event, reason, details string, result derivatives.ProcessResult) error {
	now := j.now().UTC()
	poison := derivativePoison(message, event, reason, details)
	var shouldForward bool
	var err error
	if result.State == derivatives.ProcessTerminal && event != nil {
		failure := assets.ProcessingFailure{AssetID: event.AssetID, ETag: event.ETag, ExpectedAttempt: result.Attempt, Details: result.Details}
		shouldForward, err = j.repository.FailDerivativeToPoison(ctx, failure, poison, now)
	} else {
		shouldForward, err = j.repository.RecordDerivativePoison(ctx, poison, now)
	}
	if err != nil {
		return err
	}
	if shouldForward {
		if err := j.queue.ForwardPoison(ctx, message, event, reason, now); err != nil {
			return err
		}
		if err := j.repository.MarkDerivativePoisonForwarded(ctx, poison.PoisonID, now); err != nil {
			return err
		}
	}
	return j.queue.Ack(ctx, message)
}

func derivativePoison(message Message, event *Event, reason, details string) assets.DerivativePoison {
	digest := sha256.Sum256([]byte(message.Body))
	value := assets.DerivativePoison{PoisonID: message.ID + ":" + reason, Reason: reason, Details: details, DequeueCount: message.DequeueCount, SourceMessageID: message.ID, BodySHA256: hex.EncodeToString(digest[:])}
	if event != nil {
		value.EventID, value.AssetID, value.ETag = event.EventID, event.AssetID, event.ETag
	}
	return value
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
