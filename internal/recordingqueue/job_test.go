package recordingqueue

import (
	"context"
	"errors"
	"hhc/asset-api/internal/derivativequeue"
	"sync"
	"testing"
	"time"
)

type testRepo struct {
	started, finished bool
	renewErr          error
}

func (r *testRepo) Begin(context.Context, Event) (bool, error) { r.started = true; return true, nil }
func (r *testRepo) Renew(context.Context, Event) error         { return r.renewErr }
func (r *testRepo) Finish(context.Context, Event, bool) error  { r.finished = true; return nil }

type testQueue struct {
	message        derivativequeue.Message
	acked, renewed bool
	renewErr       error
	renewedSignal  chan struct{}
	signalOnce     sync.Once
}

func (q *testQueue) Receive(context.Context, time.Duration) (derivativequeue.Message, bool, error) {
	return q.message, true, nil
}
func (q *testQueue) Ack(_ context.Context, m derivativequeue.Message) error {
	if m.PopReceipt != "new" {
		return errors.New("stale receipt")
	}
	q.acked = true
	return nil
}
func (q *testQueue) Renew(_ context.Context, m derivativequeue.Message, _ time.Duration) (derivativequeue.Message, error) {
	q.renewed = true
	if q.renewedSignal != nil {
		q.signalOnce.Do(func() { close(q.renewedSignal) })
	}
	m.PopReceipt = "new"
	return m, q.renewErr
}
func (q *testQueue) Poison(context.Context, derivativequeue.Message) error { return nil }

func TestJobRenewsLongWorkAndAcknowledgesLatestReceipt(t *testing.T) {
	r := &testRepo{}
	q := &testQueue{message: derivativequeue.Message{Body: `{"type":"asset.recording.wake.v1","version":1,"lane":"live","generation":1,"token":"t"}`, PopReceipt: "old"}, renewedSignal: make(chan struct{})}
	j := NewJob(r, q, "live", func(ctx context.Context, lane string) error {
		if lane != "live" {
			t.Fatal(lane)
		}
		select {
		case <-q.renewedSignal:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	j.renewEvery = time.Millisecond
	if err := j.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !r.finished || !q.acked || !q.renewed {
		t.Fatalf("finish=%v ack=%v renew=%v", r.finished, q.acked, q.renewed)
	}
}
func TestJobLostVisibilityCancelsWorkWithoutFinishing(t *testing.T) {
	r := &testRepo{}
	q := &testQueue{message: derivativequeue.Message{Body: `{"type":"asset.recording.wake.v1","version":1,"lane":"processing","generation":1,"token":"t"}`}, renewErr: errors.New("lost visibility")}
	cancelled := false
	j := NewJob(r, q, "processing", func(ctx context.Context, _ string) error { <-ctx.Done(); cancelled = true; return ctx.Err() })
	j.renewEvery = time.Millisecond
	if err := j.Run(context.Background()); err == nil {
		t.Fatal("lease loss hidden")
	}
	if !cancelled || r.finished || q.acked {
		t.Fatalf("cancelled=%v finished=%v ack=%v", cancelled, r.finished, q.acked)
	}
}
func TestDecodeRejectsUnknownLanesAndFields(t *testing.T) {
	for _, raw := range []string{`{"type":"asset.recording.wake.v1","version":1,"lane":"delete","generation":1,"token":"t"}`, `{"type":"asset.recording.wake.v1","version":1,"lane":"live","generation":1,"token":"t","command":"sh"}`} {
		if _, err := Decode(raw); err == nil {
			t.Fatal(raw)
		}
	}
}

func TestJobLostDatabaseOwnershipCancelsWork(t *testing.T) {
	r := &testRepo{renewErr: errors.New("database ownership lost")}
	q := &testQueue{message: derivativequeue.Message{Body: `{"type":"asset.recording.wake.v1","version":1,"lane":"live","generation":1,"token":"t"}`}}
	j := NewJob(r, q, "live", func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() })
	j.renewEvery = time.Millisecond
	if err := j.Run(context.Background()); err == nil || r.finished || q.acked {
		t.Fatal("lost DB lease committed", err)
	}
}
