package recordingqueue

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"hhc/asset-api/internal/derivativequeue"
)

type lifetimeRepo struct {
	testRepo
	blocked string
}

func (r *lifetimeRepo) Begin(ctx context.Context, e Event) (bool, error) {
	if r.blocked == "begin" {
		<-ctx.Done()
		return false, ctx.Err()
	}
	if r.blocked == "rejected_ack" {
		return false, nil
	}
	return r.testRepo.Begin(ctx, e)
}

func (r *lifetimeRepo) Finish(ctx context.Context, e Event, ok bool) error {
	if r.blocked == "finish" {
		<-ctx.Done()
		return ctx.Err()
	}
	return r.testRepo.Finish(ctx, e, ok)
}

type lifetimeQueue struct {
	testQueue
	blocked   string
	admission context.Context
}

func (q *lifetimeQueue) Receive(ctx context.Context, visibility time.Duration) (derivativequeue.Message, bool, error) {
	q.admission = ctx
	if q.blocked == "receive" {
		<-ctx.Done()
		return derivativequeue.Message{}, false, ctx.Err()
	}
	return q.testQueue.Receive(ctx, visibility)
}

func (q *lifetimeQueue) Ack(ctx context.Context, m derivativequeue.Message) error {
	if q.blocked == "ack" || q.blocked == "rejected_ack" {
		<-ctx.Done()
		return ctx.Err()
	}
	return q.testQueue.Ack(ctx, m)
}

func (q *lifetimeQueue) Poison(ctx context.Context, m derivativequeue.Message) error {
	if q.blocked == "poison" {
		<-ctx.Done()
		return ctx.Err()
	}
	return q.testQueue.Poison(ctx, m)
}

func TestJobBoundsControlIOWithoutAcknowledgingUnfinishedWork(t *testing.T) {
	for _, stage := range []string{"receive", "begin", "rejected_ack", "poison", "finish", "ack"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := &lifetimeRepo{blocked: stage}
				q := &lifetimeQueue{blocked: stage, testQueue: testQueue{message: derivativequeue.Message{
					Body: `{"type":"asset.recording.wake.v1","version":1,"lane":"live","generation":1,"token":"fixture"}`, PopReceipt: "new",
				}}}
				if stage == "poison" {
					q.message.Body = `{}`
				}
				processed := false
				j := NewJob(r, q, "live", func(context.Context, string) error { processed = true; return nil })
				ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
				defer cancel()
				started := time.Now()
				if err := j.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("expected bounded control failure, got %v", err)
				}
				if time.Since(started) >= time.Minute {
					t.Fatal("control operation used the long worker context")
				}
				if processed != (stage == "finish" || stage == "ack") || q.acked {
					t.Fatalf("processed=%v acknowledged=%v", processed, q.acked)
				}
				if r.finished != (stage == "ack") {
					t.Fatalf("unexpected durable finish=%v", r.finished)
				}
			})
		})
	}
}

func TestAdmissionDeadlineDoesNotCancelMediaOrSettlement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &lifetimeRepo{}
		q := &lifetimeQueue{testQueue: testQueue{message: derivativequeue.Message{
			Body: `{"type":"asset.recording.wake.v1","version":1,"lane":"processing","generation":1,"token":"fixture"}`, PopReceipt: "new",
		}}}
		j := NewJob(r, q, "processing", func(ctx context.Context, _ string) error {
			select {
			case <-q.admission.Done():
				return ctx.Err()
			case <-ctx.Done():
				return errors.New("admission budget leaked into media processing")
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		if err := j.Run(ctx); err != nil || !r.finished || !q.acked {
			t.Fatalf("err=%v finished=%v acknowledged=%v", err, r.finished, q.acked)
		}
	})
}

func TestControlBudgetsPreserveFullMediaAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &lifetimeRepo{}
		q := &lifetimeQueue{testQueue: testQueue{message: derivativequeue.Message{
			Body: `{"type":"asset.recording.wake.v1","version":1,"lane":"processing","generation":1,"token":"fixture"}`, PopReceipt: "new",
		}}}
		j := NewJob(r, q, "processing", func(ctx context.Context, _ string) error {
			time.Sleep(330 * time.Minute)
			return ctx.Err()
		})
		ctx, cancel := context.WithTimeout(context.Background(), 355*time.Minute)
		defer cancel()
		if err := j.Run(ctx); err != nil || !r.finished || !q.acked {
			t.Fatalf("full media budget lost: err=%v finished=%v acknowledged=%v", err, r.finished, q.acked)
		}
	})
}

func TestShutdownWaitsForMediaCleanupWithoutAcknowledging(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &lifetimeRepo{}
		q := &lifetimeQueue{testQueue: testQueue{message: derivativequeue.Message{
			Body: `{"type":"asset.recording.wake.v1","version":1,"lane":"live","generation":1,"token":"fixture"}`, PopReceipt: "new",
		}}}
		started, cleaned := make(chan struct{}), make(chan struct{})
		j := NewJob(r, q, "live", func(ctx context.Context, _ string) error {
			close(started)
			<-ctx.Done()
			time.Sleep(3 * time.Second)
			close(cleaned)
			return ctx.Err()
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- j.Run(ctx) }()
		<-started
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		select {
		case <-cleaned:
		default:
			t.Fatal("worker returned while media cleanup still owned resources")
		}
		if r.finished || q.acked {
			t.Fatal("cancelled media work was acknowledged")
		}
	})
}
