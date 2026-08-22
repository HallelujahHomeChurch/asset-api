package derivativequeue

import (
	"context"
	"errors"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/derivatives"
)

func TestJobProcessesOneExactEventAndAcknowledges(t *testing.T) {
	processor := &jobProcessor{result: derivatives.ProcessResult{State: derivatives.ProcessSatisfied}}
	queue := &jobQueue{message: derivativeMessage(1)}
	job := NewJob(processor, &jobPoisonRepository{}, queue)

	processed, err := job.RunOnce(context.Background())
	if err != nil || !processed || !queue.acked || queue.retried || processor.assetID != "asset-1" || processor.etag != "etag-1" {
		t.Fatalf("processed=%v err=%v queue=%+v processor=%+v", processed, err, queue, processor)
	}
}

func TestJobLeavesRetryableFailureUnacknowledged(t *testing.T) {
	retryAt := time.Date(2026, 8, 22, 4, 1, 0, 0, time.UTC)
	processor := &jobProcessor{result: derivatives.ProcessResult{State: derivatives.ProcessRetry, RetryAt: retryAt}}
	queue := &jobQueue{message: derivativeMessage(99)}
	job := NewJob(processor, &jobPoisonRepository{}, queue)
	job.now = func() time.Time { return retryAt.Add(-time.Minute) }

	processed, err := job.RunOnce(context.Background())
	if err != nil || !processed || queue.acked || !queue.retried || queue.poisoned || queue.retryDelay != time.Minute {
		t.Fatalf("processed=%v err=%v queue=%+v", processed, err, queue)
	}
}

func TestJobDurablyPoisonsMalformedAndTerminalMessages(t *testing.T) {
	tests := []struct {
		name      string
		message   Message
		processor *jobProcessor
	}{
		{name: "malformed", message: Message{ID: "message-1", PopReceipt: "receipt", Body: `{}`, DequeueCount: 1}, processor: &jobProcessor{}},
		{name: "terminal", message: derivativeMessage(1), processor: &jobProcessor{result: derivatives.ProcessResult{State: derivatives.ProcessTerminal, Attempt: 5, Details: "decode failed"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			queue := &jobQueue{message: test.message}
			repository := &jobPoisonRepository{shouldForward: true}
			job := NewJob(test.processor, repository, queue)
			if processed, err := job.RunOnce(context.Background()); err != nil || !processed {
				t.Fatalf("processed=%v err=%v", processed, err)
			}
			if !queue.acked || !queue.poisoned || queue.retried || !repository.recorded || !repository.marked {
				t.Fatalf("queue=%+v repository=%+v", queue, repository)
			}
			if test.name == "malformed" && test.processor.called {
				t.Fatal("malformed event reached processor")
			}
			if test.name == "terminal" && !repository.failed {
				t.Fatal("terminal failure was not committed with poison")
			}
		})
	}
}

func TestJobDoesNotForwardRecordedPoisonTwice(t *testing.T) {
	queue := &jobQueue{message: derivativeMessage(1)}
	repository := &jobPoisonRepository{}
	job := NewJob(&jobProcessor{result: derivatives.ProcessResult{State: derivatives.ProcessTerminal, Attempt: 5}}, repository, queue)

	if processed, err := job.RunOnce(context.Background()); err != nil || !processed || !queue.acked || queue.poisoned || !repository.recorded {
		t.Fatalf("processed=%v err=%v queue=%+v repository=%+v", processed, err, queue, repository)
	}
}

func TestJobRetriesUnexpectedProcessorErrorWithoutDequeueExhaustion(t *testing.T) {
	queue := &jobQueue{message: derivativeMessage(99)}
	job := NewJob(&jobProcessor{err: errors.New("database unavailable")}, &jobPoisonRepository{}, queue)

	if processed, err := job.RunOnce(context.Background()); err != nil || !processed || !queue.retried || queue.poisoned {
		t.Fatalf("processed=%v err=%v queue=%+v", processed, err, queue)
	}
}

func TestJobWaitsForDatabaseRetryAcrossRedeliveries(t *testing.T) {
	retryAt := time.Date(2026, 8, 22, 4, 1, 0, 0, time.UTC)
	processor := &jobProcessor{results: []derivatives.ProcessResult{
		{State: derivatives.ProcessRetry, RetryAt: retryAt},
		{State: derivatives.ProcessRetry, RetryAt: retryAt},
		{State: derivatives.ProcessSatisfied},
	}}
	queue := &jobQueue{message: derivativeMessage(1)}
	job := NewJob(processor, &jobPoisonRepository{}, queue)
	now := retryAt.Add(-time.Minute)
	job.now = func() time.Time { return now }

	for attempt := int64(1); attempt <= 3; attempt++ {
		queue.message.DequeueCount = attempt
		if _, err := job.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if queue.poisoned {
			t.Fatalf("attempt %d was poisoned", attempt)
		}
		now = retryAt
	}
	if !queue.acked {
		t.Fatal("satisfied redelivery was not acknowledged")
	}
}

func TestDecodeEventRejectsWrongVersionUnknownFieldsAndTrailingContent(t *testing.T) {
	for _, body := range []string{
		`{"type":"asset.derivative.requested.v1","version":2,"eventId":"event-1","assetId":"asset-1","etag":"etag-1"}`,
		`{"type":"asset.derivative.requested.v1","version":1,"eventId":"event-1","assetId":"asset-1","etag":"etag-1","extra":true}`,
		`{"type":"asset.derivative.requested.v1","version":1,"eventId":"event-1","assetId":"asset-1","etag":"etag-1"} {}`,
	} {
		if _, err := decodeEvent(body); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

type jobProcessor struct {
	assetID string
	etag    string
	called  bool
	err     error
	result  derivatives.ProcessResult
	results []derivatives.ProcessResult
}

func (p *jobProcessor) ProcessAsset(_ context.Context, assetID, etag string) (derivatives.ProcessResult, error) {
	p.assetID, p.etag, p.called = assetID, etag, true
	if len(p.results) > 0 {
		result := p.results[0]
		p.results = p.results[1:]
		return result, p.err
	}
	return p.result, p.err
}

type jobQueue struct {
	message    Message
	acked      bool
	retried    bool
	poisoned   bool
	retryDelay time.Duration
}

func (q *jobQueue) Receive(context.Context, time.Duration) (Message, bool, error) {
	return q.message, q.message.ID != "", nil
}
func (q *jobQueue) Ack(context.Context, Message) error {
	q.acked = true
	return nil
}
func (q *jobQueue) Retry(_ context.Context, _ Message, delay time.Duration) error {
	q.retried = true
	q.retryDelay = delay
	return nil
}
func (q *jobQueue) ForwardPoison(context.Context, Message, *Event, string, time.Time) error {
	q.poisoned = true
	return nil
}

type jobPoisonRepository struct {
	shouldForward bool
	recorded      bool
	failed        bool
	marked        bool
}

func (r *jobPoisonRepository) RecordDerivativePoison(_ context.Context, _ assets.DerivativePoison, _ time.Time) (bool, error) {
	r.recorded = true
	return r.shouldForward, nil
}
func (r *jobPoisonRepository) FailDerivativeToPoison(_ context.Context, _ assets.ProcessingFailure, _ assets.DerivativePoison, _ time.Time) (bool, error) {
	r.recorded, r.failed = true, true
	return r.shouldForward, nil
}
func (r *jobPoisonRepository) MarkDerivativePoisonForwarded(_ context.Context, _ string, _ time.Time) error {
	r.marked = true
	return nil
}

func derivativeMessage(count int64) Message {
	return Message{ID: "message-1", PopReceipt: "receipt", DequeueCount: count, Body: `{"type":"asset.derivative.requested.v1","version":1,"eventId":"event-1","assetId":"asset-1","etag":"etag-1"}`}
}
