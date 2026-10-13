package recordingvalidation

import (
	"context"
	"errors"
	"log/slog"
	"strings"
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

func TestPackageProgressDeadlineKeepsRenewedClaim(t *testing.T) {
	var calls []string
	err := heartbeatPackageProgress(context.Background(), func(context.Context) error {
		calls = append(calls, "heartbeat")
		return nil
	}, func(context.Context) error {
		calls = append(calls, "progress")
		return context.DeadlineExceeded
	}, slog.Default())
	if err != nil || strings.Join(calls, ",") != "heartbeat,progress" {
		t.Fatalf("renewed claim canceled or progress before renewal: %v %v", calls, err)
	}
}

func TestPackageProgressFailuresRemainFenced(t *testing.T) {
	for _, cause := range []error{assets.ErrConflict, context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			err := RunProcessingClaim(context.Background(), func(ctx context.Context) error {
				return heartbeatPackageProgress(ctx, func(context.Context) error { return cause }, func(context.Context) error {
					t.Error("progress ran without lease renewal")
					return nil
				}, slog.Default())
			}, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, func(context.Context, bool, string) error {
				t.Fatal("unowned claim committed")
				return nil
			}, time.Millisecond)
			if !errors.Is(err, cause) {
				t.Fatalf("lost cause: %v", err)
			}
		})
	}
	for _, cause := range []error{assets.ErrConflict, assets.ErrInvalidInput, context.Canceled, errors.Join(context.DeadlineExceeded, assets.ErrConflict), errors.Join(context.DeadlineExceeded, context.Canceled)} {
		err := heartbeatPackageProgress(context.Background(), func(context.Context) error { return nil }, func(context.Context) error { return cause }, slog.Default())
		if !errors.Is(err, cause) {
			t.Fatalf("progress failure ignored: %v", err)
		}
	}
}

func TestPackageProgressDeadlineRetriesWithoutRestart(t *testing.T) {
	var beats, writes, validations int
	recovered := make(chan struct{})
	finished := false
	err := RunProcessingClaim(context.Background(), func(ctx context.Context) error {
		return heartbeatPackageProgress(ctx, func(context.Context) error { beats++; return nil }, func(context.Context) error {
			writes++
			if writes == 1 {
				return packageStageError("progress_flush", context.DeadlineExceeded)
			}
			if writes == 2 {
				close(recovered)
			}
			return nil
		}, slog.Default())
	}, func(ctx context.Context) error {
		validations++
		select {
		case <-recovered:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, func(_ context.Context, ready bool, failure string) error {
		finished = ready && failure == ""
		return nil
	}, time.Millisecond)
	if err != nil || !finished || validations != 1 || beats < 2 || writes < 2 {
		t.Fatalf("same claim did not recover: err=%v ready=%v validations=%d beats=%d writes=%d", err, finished, validations, beats, writes)
	}
}
