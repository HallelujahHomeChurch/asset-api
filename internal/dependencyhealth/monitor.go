// Package dependencyhealth observes dependencies without controlling application admission.
package dependencyhealth

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Start returns a stop function that cancels the checks before the database closes.
func Start(parent context.Context, db *sql.DB, daprPort, token string, logger *slog.Logger) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		defer client.CloseIdleConnections()
		var dbObservation, daprObservation observation
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			checkDB(ctx, db, logger, &dbObservation)
			if ctx.Err() != nil {
				return
			}
			checkDapr(ctx, client, daprPort, token, logger, &daprObservation)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			logger.Warn("dependency_monitor_stop_timeout")
		}
	}
}

func checkDB(parent context.Context, db *sql.DB, logger *slog.Logger, observation *observation) {
	started := time.Now()
	phase, outcome := "acquire", "not_configured"
	if db != nil {
		ctx, cancel := context.WithTimeout(parent, 2*time.Second)
		defer cancel()
		conn, err := db.Conn(ctx)
		if err == nil {
			phase = "ping"
			err = conn.PingContext(ctx)
			_ = conn.Close()
		}
		outcome = classify(err)
	}
	attrs := []any{"dependency", "db", "phase", phase, "outcome", outcome, "elapsed_ms", time.Since(started).Milliseconds()}
	if db != nil {
		s := db.Stats()
		attrs = append(attrs, "pool_max", s.MaxOpenConnections, "pool_open", s.OpenConnections, "pool_in_use", s.InUse, "pool_wait_count", s.WaitCount)
	}
	emit(parent, logger, observation, outcome, attrs...)
}

func checkDapr(parent context.Context, client *http.Client, port, token string, logger *slog.Logger, observation *observation) {
	started := time.Now()
	outcome, status := "not_configured", 0
	if port != "" {
		outcome = "unavailable"
		n, err := strconv.Atoi(port)
		if err == nil && n > 0 && n <= 65535 {
			ctx, cancel := context.WithTimeout(parent, 2*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(n)+"/v1.0/healthz/outbound", nil)
			req.Header.Set("dapr-api-token", token)
			response, err := client.Do(req)
			outcome = classify(err)
			if response != nil {
				status = response.StatusCode
				_ = response.Body.Close()
				if status != http.StatusNoContent {
					outcome = "unavailable"
				}
			}
		}
	}
	emit(parent, logger, observation, outcome, "dependency", "dapr_outbound", "phase", "health", "outcome", outcome, "elapsed_ms", time.Since(started).Milliseconds(), "status", status)
}

func classify(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "unavailable"
	}
}

func emit(ctx context.Context, logger *slog.Logger, observation *observation, outcome string, attrs ...any) {
	if !observation.due(outcome, time.Now()) {
		return
	}
	level := slog.LevelInfo
	if outcome != "ok" && outcome != "not_configured" {
		level = slog.LevelWarn
	}
	logger.Log(ctx, level, "dependency_health", attrs...)
}

// observation is owned by the single monitor loop, independently per dependency.
type observation struct {
	outcome string
	emitted time.Time
}

func (o *observation) due(outcome string, now time.Time) bool {
	if !o.emitted.IsZero() && o.outcome == outcome && now.Sub(o.emitted) < 5*time.Minute {
		return false
	}
	o.outcome, o.emitted = outcome, now
	return true
}
