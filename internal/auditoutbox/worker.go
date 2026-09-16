package auditoutbox

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"hhc/asset-api/internal/auditclient"
)

const maxAttempts = 20

type repository interface {
	Claim(context.Context, time.Time) (*Row, error)
	Complete(context.Context, Row, string, string, time.Time, time.Time) (bool, error)
	Stats(context.Context, time.Time) (auditclient.PipelineStats, error)
}
type appender interface {
	Append(context.Context, []byte) auditclient.AppendResult
}
type Worker struct {
	store  repository
	client appender
	now    func() time.Time
}

func NewWorker(store repository, client appender) *Worker {
	return &Worker{store: store, client: client, now: time.Now}
}
func (w *Worker) Run(ctx context.Context) {
	work := time.NewTicker(time.Second)
	snapshots := time.NewTicker(time.Minute)
	defer work.Stop()
	defer snapshots.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-work.C:
			if err := w.ProcessOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("audit_dispatch_failed", "sourceService", "asset-api")
			}
		case <-snapshots.C:
			stats, err := w.store.Stats(ctx, w.now().UTC())
			if err != nil {
				slog.Error("audit_pipeline_snapshot_failed", "sourceService", "asset-api")
				continue
			}
			slog.Info("audit_pipeline_snapshot", "sourceService", "asset-api", "deliveryMode", "outbox", "pendingCount", stats.PendingCount, "oldestPendingSeconds", stats.OldestPendingSeconds, "deadLetterCount", stats.DeadLetterCount)
		}
	}
}
func (w *Worker) ProcessOnce(ctx context.Context) error {
	for range 20 {
		row, err := w.store.Claim(ctx, w.now().UTC())
		if err != nil || row == nil {
			return err
		}
		result := auditclient.AppendResult{Result: "dead_letter"}
		code := "invalid_event"
		if event, err := auditclient.Decode(row.Payload); err == nil {
			if _, hash, err := event.Canonical(); err == nil && hash == row.PayloadHash {
				result = w.client.Append(ctx, row.Payload)
				code = result.Result
				if result.Result == "retry" && row.Attempts >= maxAttempts {
					result.Result, code = "dead_letter", "dead_letter"
				}
			}
		}
		level := slog.LevelInfo
		if result.Result == "conflict" {
			level = slog.LevelError
		}
		slog.Log(ctx, level, "audit_append_result", "sourceService", "asset-api", "result", result.Result, "httpStatus", result.HTTPStatus, "durationMs", result.DurationMs)
		now, status := w.now().UTC(), "dead_letter"
		available := now
		switch result.Result {
		case "accepted", "duplicate":
			status, code = "delivered", ""
		case "retry":
			status = "pending"
			delay := retryDelay(row.Attempts)
			available = now.Add(delay + time.Duration(rand.Int64N(int64(delay/5)+1)))
		case "conflict":
			code = "conflict"
		default:
			if code != "invalid_event" {
				code = "dead_letter"
			}
		}
		completed, err := w.store.Complete(ctx, *row, status, code, available, now)
		if err != nil {
			return err
		}
		if !completed {
			return errors.New("audit outbox transition lost")
		}
	}
	return nil
}
func retryDelay(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 30 * time.Second
	case 2:
		return 2 * time.Minute
	case 3:
		return 5 * time.Minute
	case 4:
		return 15 * time.Minute
	case 5:
		return time.Hour
	default:
		return 6 * time.Hour
	}
}
