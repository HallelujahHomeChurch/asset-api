package postgres

import (
	"context"
	"errors"
	"hhc/asset-api/internal/assets"
	"testing"
)

func TestRecordingSessionLockExcludesOtherProcessAndReleases(t *testing.T) {
	db := isolatedIntegrationDB(t)
	first, second := newRecordingSessionStore(db), newRecordingSessionStore(db)
	ctx := context.Background()
	err := first.WithSessionLock(ctx, "same-session", func(ctx context.Context) error {
		err := second.WithSessionLock(ctx, "same-session", func(context.Context) error { t.Error("concurrent mutation entered"); return nil })
		if !errors.Is(err, assets.ErrConflict) {
			t.Fatalf("expected lock conflict: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.WithSessionLock(ctx, "same-session", func(context.Context) error { return nil }); err != nil {
		t.Fatalf("lock leaked: %v", err)
	}
}
