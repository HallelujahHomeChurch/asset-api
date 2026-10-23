package diagnostics

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentAccountingAndBoundedFields(t *testing.T) {
	var r Recorder
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			for n := 0; n < 100; n++ {
				r.Observe(GrantAuthority, time.Now().Add(-2*time.Second), context.DeadlineExceeded)
			}
		})
	}
	wg.Wait()
	r.Observe(ProjectionSQL, time.Now(), errors.New("token=secret https://signed.invalid/?sig=secret SQL user-id"))
	rows := r.Snapshot()
	if rows[GrantAuthority][0] != 2000 || rows[GrantAuthority][1] != 2000 || rows[GrantAuthority][2] != 2000 || rows[GrantAuthority][7] != 0 {
		t.Fatalf("lost/corrupt accounting: %v", rows)
	}
	if r.Snapshot() != ([stageCount][8]uint64{}) {
		t.Fatal("snapshot did not reset")
	}
	var out bytes.Buffer
	logStages(slog.New(slog.NewJSONHandler(&out, nil)), 2*time.Minute, rows)
	for _, secret := range []string{"secret", "signed.invalid", "user-id", "SQL"} {
		if strings.Contains(out.String(), secret) {
			t.Fatal("sensitive data emitted")
		}
	}
	// Absolute serialization bound, even with every counter at its type maximum.
	for i := range rows {
		for j := range rows[i] {
			rows[i][j] = math.MaxUint64
		}
	}
	out.Reset()
	logStages(slog.New(slog.NewJSONHandler(&out, nil)), 2*time.Minute, rows)
	if out.Len() > 2048 {
		t.Fatalf("summary exceeds byte budget: %d", out.Len())
	}
	t.Logf("worst-case stage summary bytes: %d", out.Len())
	out.Reset()
	logStages(slog.New(slog.NewTextHandler(&out, nil)), 2*time.Minute, rows)
	if out.Len() > 2048 {
		t.Fatalf("text summary exceeds budget: %d", out.Len())
	}
}

func TestHistogramBoundariesAndOpaqueErrors(t *testing.T) {
	var r Recorder
	r.Observe(ProjectionSQL, time.Now().Add(-50*time.Millisecond), errors.New("cookie=SECRET"))
	row := r.Snapshot()[ProjectionSQL]
	if row[0] != 1 || row[1] != 1 || row[2] != 0 || row[5] != 0 || row[6] != 1 || row[7] != 1 {
		t.Fatalf("wrong buckets: %v", row)
	}
}

type synchronizedBuffer struct {
	sync.Mutex
	bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.Write(p)
}
func TestPeriodicPoolDeltasAndShutdown(t *testing.T) {
	var r Recorder
	var out synchronizedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	calls := 0
	stats := func() sql.DBStats {
		calls++
		return sql.DBStats{MaxOpenConnections: 10, OpenConnections: 8, InUse: 7, Idle: 1, WaitCount: int64(calls * 2), WaitDuration: time.Duration(calls) * 3 * time.Millisecond}
	}
	go func() {
		r.run(ctx, stats, slog.New(slog.NewJSONHandler(&out, nil)), time.Millisecond, 2*time.Millisecond)
		close(done)
	}()
	deadline := time.After(time.Second)
	for {
		out.Lock()
		ready := strings.Contains(out.Buffer.String(), "media_stage_summary")
		out.Unlock()
		if ready {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no summary")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("summary loop did not stop")
	}
	out.Lock()
	defer out.Unlock()
	for _, line := range strings.Split(strings.TrimSpace(out.Buffer.String()), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event["msg"] == "db_pool_summary" {
			if event["wait_count"] != float64(2) || event["wait_ms"] != float64(3) {
				t.Fatalf("not interval deltas: %s", line)
			}
			if len(line)+1 > 512 {
				t.Fatalf("pool summary exceeds budget: %d", len(line)+1)
			}
		}
	}
}

type blockedSink struct {
	entered chan struct{}
	release chan struct{}
}

func (s *blockedSink) Write(p []byte) (int, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.release
	return 0, errors.New("sink unavailable")
}
func TestUnavailableSinkDoesNotBlockObservations(t *testing.T) {
	var r Recorder
	sink := &blockedSink{make(chan struct{}, 1), make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.run(ctx, func() sql.DBStats { return sql.DBStats{} }, slog.New(slog.NewTextHandler(sink, nil)), time.Millisecond, time.Hour)
		close(done)
	}()
	select {
	case <-sink.entered:
	case <-time.After(time.Second):
		t.Fatal("sink not called")
	}
	observed := make(chan struct{})
	go func() {
		r.Observe(GrantAuthority, time.Now(), errors.New("https://private.invalid?token=secret"))
		r.Transition(context.Background(), RecoveryActivated, fixtureCapture, 1, 2, -1, time.Time{})
		close(observed)
	}()
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("console blocked request observation")
	}
	cancel()
	close(sink.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not stop after sink returned")
	}
}
