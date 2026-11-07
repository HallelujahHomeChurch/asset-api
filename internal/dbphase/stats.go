package dbphase

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"
)

// StartPoolStats owns a bounded global pressure signal, never request attribution.
// Call stop after work drains and before closing the pool, including early exits.
func StartPoolStats(parent context.Context, stats func() sql.DBStats) func() {
	return startPoolStats(parent, stats, 15*time.Second)
}
func startPoolStats(parent context.Context, stats func() sql.DBStats, interval time.Duration) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	previous := stats()
	flush := func() {
		current := stats()
		waits := max(current.WaitCount-previous.WaitCount, 0)
		duration := max(current.WaitDuration-previous.WaitDuration, 0)
		previous = current
		nearFull := current.MaxOpenConnections > 0 && current.InUse*10 >= current.MaxOpenConnections*9
		if waits == 0 && duration == 0 && !nearFull {
			return
		}
		slog.Error("database pool pressure", "scope", "global", "phase", "db_pool", "max", current.MaxOpenConnections, "open", current.OpenConnections, "in_use", current.InUse, "idle", current.Idle, "wait_count_delta", waits, "wait_ms_delta", duration.Milliseconds())
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				flush()
				return
			case <-ticker.C:
				flush()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(cancel)
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
		}
	}
}
