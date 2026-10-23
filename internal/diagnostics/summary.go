// Package diagnostics records bounded, in-memory stage aggregates only.
package diagnostics

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"
)

type Stage uint8

const (
	GrantSQL Stage = iota
	GrantAuthority
	CaptureBegin
	CaptureOwnerLock
	CaptureRow
	CaptureCallback
	CaptureCommit
	ProjectionSQL
	ProjectionAuthority
	ProjectionReuse
	CaptureLiveRow
	stageCount
)

// Rows are count, error, cancellation, sum_ms, max_ms, <=10ms, <=100ms, <=1000ms.
// Buckets are cumulative. Slower-than-1000ms is count minus the final bucket.
type Recorder struct {
	mu         sync.Mutex
	rows       [stageCount][8]uint64
	events     [32]event
	eventCount int
	eventStats [4]uint64
}

var Default Recorder

func (r *Recorder) Observe(stage Stage, started time.Time, err error) {
	if stage >= stageCount {
		return
	}
	elapsed := max(time.Since(started).Milliseconds(), 0)
	r.mu.Lock()
	defer r.mu.Unlock()
	row := &r.rows[stage]
	row[0]++
	if err != nil {
		row[1]++
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		row[2]++
	}
	row[3] += uint64(elapsed)
	row[4] = max(row[4], uint64(elapsed))
	for i, limit := range []int64{10, 100, 1000} {
		if elapsed <= limit {
			row[5+i]++
		}
	}
}

// Snapshot atomically drains the completed-stage interval counters.
func (r *Recorder) Snapshot() [stageCount][8]uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows := r.rows
	r.rows = [stageCount][8]uint64{}
	return rows
}

// Run never emits from a request or while holding the recorder lock. No provider
// calls are added: Stats reads database/sql's local counters. Each invocation is
// a fresh counter baseline, scoped by the platform revision/replica log envelope.
func (r *Recorder) Run(ctx context.Context, stats func() sql.DBStats, logger *slog.Logger) {
	r.run(ctx, stats, logger, 15*time.Second, 2*time.Minute)
}
func (r *Recorder) run(ctx context.Context, stats func() sql.DBStats, logger *slog.Logger, dbInterval, stageInterval time.Duration) {
	dbTick, stageTick := time.NewTicker(dbInterval), time.NewTicker(stageInterval)
	defer dbTick.Stop()
	defer stageTick.Stop()
	previous := stats()
	last := time.Now()
	lastStages := last
	for {
		select {
		case <-ctx.Done():
			return
		case <-dbTick.C:
			if ctx.Err() != nil {
				return
			}
			now, current := time.Now(), stats()
			logger.Info("db_pool_summary", "interval_ms", now.Sub(last).Milliseconds(), "max", current.MaxOpenConnections, "open", current.OpenConnections, "in_use", current.InUse, "idle", current.Idle, "wait_count", max(current.WaitCount-previous.WaitCount, 0), "wait_ms", max((current.WaitDuration-previous.WaitDuration).Milliseconds(), 0))
			previous, last = current, now
			r.flushEvents(ctx, logger)
		case <-stageTick.C:
			if ctx.Err() != nil {
				return
			}
			now := time.Now()
			logStages(logger, now.Sub(lastStages), r.Snapshot())
			lastStages = now
			counts := r.eventSnapshot()
			logger.Info("recording_authority_delivery", "written", counts[0], "queue_dropped", counts[1], "write_failed", counts[2], "filtered_or_shutdown", counts[3])
		}
	}
}

func logStages(logger *slog.Logger, interval time.Duration, rows [stageCount][8]uint64) {
	// A fixed integer matrix cannot fail JSON encoding; preserve parseable arrays
	// even when the existing service logger uses slog.TextHandler.
	encoded, _ := json.Marshal(rows)
	logger.Info("media_stage_summary", "schema", 1, "interval_ms", interval.Milliseconds(), "rows_json", string(encoded))
}
