package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"time"

	"hhc/asset-api/internal/assets"
)

type recordingSessionStore struct{ db *sql.DB }

func recordingTransitionResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return assets.ErrConflict
	}
	return nil
}

type recordingLockConnKey struct{}

type recordingStatements interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *recordingSessionStore) statements(ctx context.Context) recordingStatements {
	if conn, ok := ctx.Value(recordingLockConnKey{}).(*sql.Conn); ok {
		return conn
	}
	return s.db
}

func newRecordingSessionStore(db *sql.DB) *recordingSessionStore {
	return &recordingSessionStore{db: db}
}

// Serialize R2 completion and deletion across API/worker processes. A failed
// contender retries instead of holding every pool connection waiting for a lock.
func (s *recordingSessionStore) WithSessionLock(ctx context.Context, id string, run func(context.Context) error) (resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// Shared across package grants/cleanup and source completion; updates take
	// the exclusive counterpart before touching any package or cover row.
	var policyLocked bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock_shared(`+retentionLock+`)`).Scan(&policyLocked); err != nil {
		return err
	}
	if !policyLocked {
		return assets.ErrConflict
	}
	defer func() {
		unlockCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		var released bool
		err := conn.QueryRowContext(unlockCtx, `SELECT pg_advisory_unlock_shared(`+retentionLock+`)`).Scan(&released)
		if err != nil || !released {
			if err == nil {
				err = errors.New("recording policy lock was not held")
			}
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			resultErr = errors.Join(resultErr, err)
		}
	}()
	var acquired bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended('recording-session:' || $1,0))`, id).Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return assets.ErrConflict
	}
	defer func() {
		unlockCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		var released bool
		unlockErr := conn.QueryRowContext(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended('recording-session:' || $1,0))`, id).Scan(&released)
		if unlockErr != nil || !released {
			if unlockErr == nil {
				unlockErr = errors.New("recording session lock was not held")
			}
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			resultErr = errors.Join(resultErr, unlockErr)
		}
	}()
	return run(context.WithValue(ctx, recordingLockConnKey{}, conn))
}
