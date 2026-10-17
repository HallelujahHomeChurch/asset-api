package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDisabledHLSDoesNotStartLegacyProcessing(t *testing.T) {
	t.Setenv("ASSET_RECORDING_HLS_ENABLED", "false")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("ASSET_R2_ACCESS_KEY_ID", "")
	if err := run(context.Background()); err != nil {
		t.Fatalf("disabled worker accessed runtime dependencies: %v", err)
	}
}

func TestDrainLiveWaitsForIncomingFragments(t *testing.T) {
	calls := 0
	err := drainLiveValidation(context.Background(), time.Now().Add(time.Second), time.Millisecond, func(context.Context) (bool, error) { calls++; return calls == 2, nil }, func(context.Context) (bool, error) { return calls < 2, nil })
	if err != nil || calls != 3 {
		t.Fatalf("missed arriving segment: calls=%d err=%v", calls, err)
	}
}
func TestDrainLiveIdleAndCancellation(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "active"}[pending], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			err := drainLiveValidation(ctx, time.Now().Add(time.Second), time.Millisecond, func(context.Context) (bool, error) {
				calls++
				if pending {
					cancel()
				}
				return false, nil
			}, func(context.Context) (bool, error) { return pending, nil })
			if pending && !errors.Is(err, context.Canceled) || !pending && err != nil || calls != 1 {
				t.Fatalf("unexpected wait: %d %v", calls, err)
			}
		})
	}
}
