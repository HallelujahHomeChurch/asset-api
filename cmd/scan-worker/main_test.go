package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunLoopContinuesImmediatelyAfterWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32

	err := runLoop(ctx, func(context.Context) (bool, error) {
		if calls.Add(1) == 2 {
			cancel()
		}
		return true, nil
	}, func(context.Context) (bool, error) { return false, nil }, time.Hour)

	if err != nil || calls.Load() != 2 {
		t.Fatalf("runLoop() = %v after %d calls, want nil after 2 immediate calls", err, calls.Load())
	}
}

func TestRunLoopWaitsAfterEmpty(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	started := time.Now()

	err := runLoop(ctx, func(context.Context) (bool, error) {
		if calls.Add(1) == 2 {
			cancel()
		}
		return false, nil
	}, func(context.Context) (bool, error) { return false, nil }, 20*time.Millisecond)

	if err != nil || calls.Load() != 2 || time.Since(started) < 20*time.Millisecond {
		t.Fatalf("runLoop() = %v after %d calls in %s, want one idle wait", err, calls.Load(), time.Since(started))
	}
}

func TestRunLoopReturnsWorkerError(t *testing.T) {
	want := errors.New("scan failed")
	if err := runLoop(context.Background(), func(context.Context) (bool, error) { return false, want }, func(context.Context) (bool, error) { return false, nil }, time.Second); !errors.Is(err, want) {
		t.Fatalf("runLoop() error = %v, want %v", err, want)
	}
}

func TestRunLoopCancellationIsClean(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false

	if err := runLoop(ctx, func(context.Context) (bool, error) { called = true; return false, nil }, func(context.Context) (bool, error) { return false, nil }, time.Hour); err != nil || called {
		t.Fatalf("runLoop() = %v, called = %v; want nil without work", err, called)
	}
}

func TestRunLoopConsumesWarmPulseAfterEmptyScan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var scans, pulses atomic.Int32

	err := runLoop(ctx,
		func(context.Context) (bool, error) { scans.Add(1); return false, nil },
		func(context.Context) (bool, error) {
			if pulses.Add(1) == 2 {
				cancel()
			}
			return true, nil
		},
		time.Hour,
	)

	if err != nil || scans.Load() != 2 || pulses.Load() != 2 {
		t.Fatalf("runLoop() = %v after scans=%d pulses=%d, want two immediate pulse drains", err, scans.Load(), pulses.Load())
	}
}

func TestDeploymentPassesWarmQueueToWorker(t *testing.T) {
	template, err := os.ReadFile("../../infra/main.bicep")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(template), "{ name: 'ASSET_SCAN_WARM_QUEUE_URL', value: 'https://${storageAccount.name}.queue.${az.environment().suffixes.storage}/asset-scan-warm' }") != 3 {
		t.Fatal("asset-scan-worker must receive ASSET_SCAN_WARM_QUEUE_URL")
	}
	if !strings.Contains(string(template), "resource scanWarmQueueProcessor") || strings.Count(string(template), "dependsOn: [acrPull, scanSecretAccess, scanQueueProcessor, scanQueueReader, scanWarmQueueReader, scanWarmQueueProcessor") != 2 {
		t.Fatal("asset-scan-worker must be allowed to delete warm pulses")
	}
}
