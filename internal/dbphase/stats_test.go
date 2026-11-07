package dbphase

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPoolPressureGlobalFinalFlushAndStop(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	var mu sync.Mutex
	state := sql.DBStats{MaxOpenConnections: 2}
	var calls atomic.Int32
	stats := func() sql.DBStats { calls.Add(1); mu.Lock(); defer mu.Unlock(); return state }
	stop := StartPoolStats(context.Background(), stats)
	mu.Lock()
	state.WaitCount = 1
	state.WaitDuration = 50 * time.Millisecond
	mu.Unlock()
	stop()
	stop()
	if strings.Count(logs.String(), "database pool pressure") != 1 || !strings.Contains(logs.String(), `"scope":"global"`) || !strings.Contains(logs.String(), `"wait_count_delta":1`) || !strings.Contains(logs.String(), `"wait_ms_delta":50`) || strings.Contains(logs.String(), "trace_id") {
		t.Fatal(logs.String())
	}
	count := calls.Load()
	time.Sleep(5 * time.Millisecond)
	if calls.Load() != count {
		t.Fatal("recorder leaked after stop")
	}
	logs.Reset()
	stop = StartPoolStats(context.Background(), stats)
	stop()
	if logs.Len() != 0 {
		t.Fatal("unchanged healthy pool emitted")
	}
}
func TestPoolPressurePeriodicNearFull(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	sampled := make(chan struct{}, 2)
	var count atomic.Int32
	stop := startPoolStats(context.Background(), func() sql.DBStats {
		if count.Add(1) > 1 {
			select {
			case sampled <- struct{}{}:
			default:
			}
		}
		return sql.DBStats{MaxOpenConnections: 2, OpenConnections: 2, InUse: 2}
	}, time.Millisecond)
	select {
	case <-sampled:
	case <-time.After(time.Second):
		t.Fatal("periodic sample missing")
	}
	stop()
	if !strings.Contains(logs.String(), `"in_use":2`) {
		t.Fatal("near-full signal missing")
	}
}

type blockedPoolHandler struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *blockedPoolHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *blockedPoolHandler) Handle(context.Context, slog.Record) error {
	h.once.Do(func() { close(h.entered) })
	<-h.release
	return nil
}
func (h *blockedPoolHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *blockedPoolHandler) WithGroup(string) slog.Handler      { return h }
func TestPoolStopBoundsBlockedLogHandler(t *testing.T) {
	h := &blockedPoolHandler{entered: make(chan struct{}), release: make(chan struct{})}
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(old)
	stop := startPoolStats(context.Background(), func() sql.DBStats { return sql.DBStats{MaxOpenConnections: 1, InUse: 1} }, time.Millisecond)
	select {
	case <-h.entered:
	case <-time.After(time.Second):
		close(h.release)
		t.Fatal("handler never reached")
	}
	started := time.Now()
	stop()
	elapsed := time.Since(started)
	close(h.release)
	stop()
	stop()
	if elapsed > 750*time.Millisecond {
		t.Fatalf("stop waited on blocked logger: %s", elapsed)
	}
}
