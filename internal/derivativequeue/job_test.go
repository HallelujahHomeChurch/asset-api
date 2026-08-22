package derivativequeue

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestJobProcessesOneExactEventAndAcknowledges(t *testing.T) {
	processor := &jobProcessor{}
	queue := &jobQueue{message: derivativeMessage(1)}
	job := NewJob(processor, queue, 5)

	processed, err := job.RunOnce(context.Background())
	if err != nil || !processed || !queue.acked || queue.retried || processor.assetID != "asset-1" || processor.etag != "etag-1" {
		t.Fatalf("processed=%v err=%v queue=%+v processor=%+v", processed, err, queue, processor)
	}
}

func TestJobLeavesRetryableFailureUnacknowledged(t *testing.T) {
	processor := &jobProcessor{err: errors.New("blob unavailable")}
	queue := &jobQueue{message: derivativeMessage(1)}
	job := NewJob(processor, queue, 5)

	processed, err := job.RunOnce(context.Background())
	if err != nil || !processed || queue.acked || !queue.retried || queue.poisoned {
		t.Fatalf("processed=%v err=%v queue=%+v", processed, err, queue)
	}
}

func TestJobPoisonsMalformedAndExhaustedMessages(t *testing.T) {
	tests := []struct {
		name      string
		message   Message
		processor *jobProcessor
	}{
		{name: "malformed", message: Message{ID: "message-1", PopReceipt: "receipt", Body: `{}`, DequeueCount: 1}, processor: &jobProcessor{}},
		{name: "exhausted", message: derivativeMessage(5), processor: &jobProcessor{err: errors.New("processing failed")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			queue := &jobQueue{message: test.message}
			job := NewJob(test.processor, queue, 5)
			if processed, err := job.RunOnce(context.Background()); err != nil || !processed {
				t.Fatalf("processed=%v err=%v", processed, err)
			}
			if !queue.acked || !queue.poisoned || queue.retried {
				t.Fatalf("queue=%+v", queue)
			}
			if test.name == "malformed" && test.processor.called {
				t.Fatal("malformed event reached processor")
			}
		})
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
}

func (p *jobProcessor) ProcessAsset(_ context.Context, assetID, etag string) error {
	p.assetID, p.etag, p.called = assetID, etag, true
	return p.err
}

type jobQueue struct {
	message  Message
	acked    bool
	retried  bool
	poisoned bool
}

func (q *jobQueue) Receive(context.Context, time.Duration) (Message, bool, error) {
	return q.message, q.message.ID != "", nil
}
func (q *jobQueue) Ack(context.Context, Message) error {
	q.acked = true
	return nil
}
func (q *jobQueue) Retry(context.Context, Message, time.Duration) error {
	q.retried = true
	return nil
}
func (q *jobQueue) ForwardPoison(context.Context, Message, *Event, string, time.Time) error {
	q.poisoned = true
	return nil
}

func derivativeMessage(count int64) Message {
	return Message{ID: "message-1", PopReceipt: "receipt", DequeueCount: count, Body: `{"type":"asset.derivative.requested.v1","version":1,"eventId":"event-1","assetId":"asset-1","etag":"etag-1"}`}
}
