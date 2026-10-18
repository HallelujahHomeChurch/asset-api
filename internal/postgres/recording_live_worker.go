package postgres

import (
	"context"
	"database/sql/driver"
	"errors"
	"time"
)

// Elect only the standby worker; individual claims still own the existing fenced
// processing slots. Other scheduled executions remain free to process non-live work.
func (s *RecordingCaptureStore) WithLiveWorker(ctx context.Context, run func(context.Context, func(context.Context) error) error) (acquired bool, result error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	const key = `hashtextextended('recording-live-worker',0)`
	if err = conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(`+key+`)`).Scan(&acquired); err != nil || !acquired {
		return acquired, err
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var released bool
		err := conn.QueryRowContext(unlockCtx, `SELECT pg_advisory_unlock(`+key+`)`).Scan(&released)
		if err != nil || !released {
			if err == nil {
				err = errors.New("live worker lock was not held")
			}
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			result = errors.Join(result, err)
		}
	}()
	// Check the same session before every claim so a lost lock connection stops
	// admission; in-flight media remains protected by its existing fenced lease.
	return true, run(ctx, conn.PingContext)
}
