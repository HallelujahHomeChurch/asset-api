package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"hhc/asset-api/internal/meetingclient"
)

func TestRunSendsPulseBatchInsideLeadAndTail(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	for _, window := range []meetingclient.Window{
		{StartsAt: now.Add(5 * time.Minute), EndsAt: now.Add(time.Hour)},
		{StartsAt: now.Add(-time.Hour), EndsAt: now.Add(-10 * time.Minute)},
	} {
		sent := 0
		err := run(context.Background(), now, 5*time.Minute, 10*time.Minute,
			func(context.Context, time.Time, time.Time) ([]meetingclient.Window, error) {
				return []meetingclient.Window{window, window}, nil
			},
			func(context.Context) error { sent++; return nil })
		if err != nil || sent != pulseDepth {
			t.Fatalf("window=%+v sent=%d err=%v", window, sent, err)
		}
	}
}

func TestRunDoesNotSendOutsideWindow(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	sent := 0
	err := run(context.Background(), now, 5*time.Minute, 10*time.Minute,
		func(context.Context, time.Time, time.Time) ([]meetingclient.Window, error) {
			return []meetingclient.Window{{StartsAt: now.Add(5*time.Minute + time.Nanosecond), EndsAt: now.Add(time.Hour)}}, nil
		}, func(context.Context) error { sent++; return nil })
	if err != nil || sent != 0 {
		t.Fatalf("sent=%d err=%v", sent, err)
	}
}

func TestRunReturnsQueryAndPulseFailures(t *testing.T) {
	want := errors.New("failed")
	now := time.Now()
	if err := run(context.Background(), now, time.Minute, time.Minute, func(context.Context, time.Time, time.Time) ([]meetingclient.Window, error) { return nil, want }, func(context.Context) error { return nil }); !errors.Is(err, want) {
		t.Fatalf("query err=%v", err)
	}
	if err := run(context.Background(), now, time.Minute, time.Minute, func(context.Context, time.Time, time.Time) ([]meetingclient.Window, error) {
		return []meetingclient.Window{{StartsAt: now, EndsAt: now}}, nil
	}, func(context.Context) error { return want }); !errors.Is(err, want) {
		t.Fatalf("pulse err=%v", err)
	}
}
