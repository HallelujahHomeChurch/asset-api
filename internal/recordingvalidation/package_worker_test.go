package recordingvalidation

import (
	"context"
	"errors"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
)

func TestPackageWorkerLeaseLossCancelsAndNeverCommits(t *testing.T) {
	err := RunProcessingClaim(context.Background(), func(context.Context) error { return assets.ErrConflict }, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, func(context.Context, bool, string) error { t.Fatal("stale worker committed"); return nil }, time.Millisecond)
	if !errors.Is(err, assets.ErrConflict) {
		t.Fatalf("lost lease: %v", err)
	}
}

func TestPackageWorkerReadyAndFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		err     error
		ready   bool
		failure string
	}{{nil, true, ""}, {assets.ErrInvalidUpload, false, "invalid"}, {assets.ErrRecordingPackageTooLarge, false, "invalid"}, {assets.ErrRecordingPackageEstimateTooLarge, false, "invalid"}, {errors.New("provider transient"), false, "retry"}} {
		called := false
		err := RunProcessingClaim(context.Background(), func(context.Context) error { return nil }, func(context.Context) error { return tc.err }, func(_ context.Context, ready bool, failure string) error {
			called = true
			if ready != tc.ready || failure != tc.failure {
				t.Fatalf("finish: %t %s", ready, failure)
			}
			return nil
		}, time.Hour)
		if err != nil || !called {
			t.Fatalf("finish omitted: %v", err)
		}
	}
}
