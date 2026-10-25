package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hhc/asset-api/internal/assets"
	"testing"
	"time"
)

func TestRecordingSessionLockExcludesOtherProcessAndReleases(t *testing.T) {
	db := isolatedIntegrationDB(t)
	first, second := newRecordingSessionStore(db), newRecordingSessionStore(db)
	ctx := context.Background()
	err := first.WithSessionLock(ctx, "same-session", func(ctx context.Context, _ *sql.Conn) error {
		err := second.WithSessionLock(ctx, "same-session", func(context.Context, *sql.Conn) error { t.Error("concurrent mutation entered"); return nil })
		if !errors.Is(err, assets.ErrConflict) {
			t.Fatalf("expected lock conflict: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.WithSessionLock(ctx, "same-session", func(context.Context, *sql.Conn) error { return nil }); err != nil {
		t.Fatalf("lock leaked: %v", err)
	}
}

func TestRecordingSessionLockCancellationAndErrorRelease(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%v", canceled), func(t *testing.T) {
			db := isolatedIntegrationDB(t)
			db.SetMaxOpenConns(1)
			store := newRecordingSessionStore(db)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			expected := errors.New("callback failed")
			var pid int
			err := store.WithSessionLock(ctx, "release-proof", func(ctx context.Context, conn *sql.Conn) error {
				if err := conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
					return err
				}
				if canceled {
					cancel()
					return ctx.Err()
				}
				return expected
			})
			if canceled {
				expected = context.Canceled
			}
			if !errors.Is(err, expected) {
				t.Fatalf("callback error lost: %v", err)
			}
			checkCtx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			var locks int
			if err := db.QueryRowContext(checkCtx, `SELECT count(*) FROM pg_locks WHERE pid=$1 AND locktype='advisory'`, pid).Scan(&locks); err != nil {
				t.Fatal(err)
			}
			if locks != 0 {
				t.Fatalf("session/policy locks leaked after callback: %d", locks)
			}
			if err := store.WithSessionLock(checkCtx, "release-proof", func(context.Context, *sql.Conn) error { return nil }); err != nil {
				t.Fatalf("reacquisition: %v", err)
			}
			if db.Stats().InUse != 0 || db.Stats().WaitCount != 0 {
				t.Fatalf("pool lease leaked or nested checkout: %+v", db.Stats())
			}
		})
	}
}
