package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunOnceValidatesAndCleansUpBeforeExiting(t *testing.T) {
	validationFailure := errors.New("validation unavailable")
	cleanupFailure := errors.New("delete unavailable")
	validationCalls := 0
	deleted := []string{}
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	err := runOnce(context.Background(),
		func(context.Context) error {
			validationCalls++
			return validationFailure
		},
		func(_ context.Context, at time.Time) ([]string, error) {
			if !at.Equal(now) {
				t.Fatalf("cleanup queried at %s, want %s", at, now)
			}
			return []string{"version-1", "version-2"}, nil
		},
		func(_ context.Context, versionID string, at time.Time) error {
			if !at.Equal(now) {
				t.Fatalf("cleanup called at %s, want %s", at, now)
			}
			deleted = append(deleted, versionID)
			if versionID == "version-1" {
				return cleanupFailure
			}
			return nil
		},
		func() time.Time { return now },
	)
	if validationCalls != 1 || len(deleted) != 2 || deleted[0] != "version-1" || deleted[1] != "version-2" {
		t.Fatalf("expected one validation and both deletions, got validation=%d deleted=%v", validationCalls, deleted)
	}
	if !errors.Is(err, validationFailure) || !errors.Is(err, cleanupFailure) {
		t.Fatalf("expected both failures to reach the scheduler, got %v", err)
	}
}
