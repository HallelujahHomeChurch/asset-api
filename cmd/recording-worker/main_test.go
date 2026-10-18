package main

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
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
	err := drainLiveValidation(context.Background(), time.Now().Add(time.Second), time.Millisecond, func(context.Context) (bool, error) { calls++; return calls == 2, nil }, func(context.Context) (bool, error) { return calls < 2, nil }, func(context.Context) bool { return false })
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
			}, func(context.Context) (bool, error) { return pending, nil }, func(context.Context) bool { return false })
			if pending && !errors.Is(err, context.Canceled) || !pending && err != nil || calls != 1 {
				t.Fatalf("unexpected wait: %d %v", calls, err)
			}
		})
	}
}

func TestDrainLiveStandbyHonorsDeadline(t *testing.T) {
	started := time.Now()
	calls := 0
	err := drainLiveValidation(context.Background(), started.Add(20*time.Millisecond), time.Millisecond, func(context.Context) (bool, error) { calls++; return false, nil }, func(context.Context) (bool, error) { return true, nil }, func(context.Context) bool { return false })
	if err != nil || calls < 1 || time.Since(started) < 20*time.Millisecond || time.Since(started) > time.Second {
		t.Fatalf("standby escaped its bounded window: calls=%d elapsed=%s err=%v", calls, time.Since(started), err)
	}
}

func TestLiveOwnerBridgesCronBoundariesWithoutCancellingClaims(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		next := start.Add(10 * time.Second)
		claims := 0
		deadline := start.Add(liveWorkerWindow)
		err := drainLiveValidation(context.Background(), deadline, 5*time.Second, func(context.Context) (bool, error) {
			if claims == 17 || time.Now().Before(next) {
				return false, nil
			}
			if wait := time.Since(next); wait > 5*time.Second {
				t.Fatalf("fragment waited %s across cron boundary", wait)
			}
			time.Sleep(8 * time.Second)
			claims++
			next = next.Add(10 * time.Second)
			return true, nil
		}, func(context.Context) (bool, error) { return time.Since(start) < 3*time.Minute, nil }, func(context.Context) bool { return false })
		if err != nil || claims != 17 {
			t.Fatalf("continuous service: claims=%d err=%v", claims, err)
		}
		t.Logf("simulated live interval=%s claims=%d processing=%s", time.Since(start), claims, time.Duration(claims)*8*time.Second)
	})
}

// A background validation holds slot 1 throughout; optional media must reuse
// slot 2 between live fragments instead of waiting for the live session to end.
func TestLiveOwnerServicesOptionalMediaBetweenFragments(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		next := start
		fragments, images := 0, 0
		err := drainLiveValidation(context.Background(), start.Add(time.Minute), time.Second,
			func(context.Context) (bool, error) {
				if fragments == 3 || time.Now().Before(next) {
					return false, nil
				}
				time.Sleep(5 * time.Second)
				fragments++
				next = next.Add(10 * time.Second)
				return true, nil
			}, func(context.Context) (bool, error) { return time.Since(start) < 30*time.Second, nil },
			func(context.Context) bool {
				if images == fragments {
					return false
				}
				if fragments == 0 {
					t.Fatal("optional media ran before live verification")
				}
				time.Sleep(time.Second)
				images++
				return true
			})
		if err != nil || fragments != 3 || images != 3 {
			t.Fatalf("live=%d images=%d err=%v", fragments, images, err)
		}
	})
}
