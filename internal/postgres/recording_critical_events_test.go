package postgres

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/diagnostics"
)

type authorityEventSink struct {
	mu     sync.Mutex
	events []slog.Record
}

func (s *authorityEventSink) Enabled(context.Context, slog.Level) bool { return true }
func (s *authorityEventSink) Handle(_ context.Context, r slog.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Message == "recording_authority_committed" {
		s.events = append(s.events, r.Clone())
	}
	return nil
}
func (s *authorityEventSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *authorityEventSink) WithGroup(string) slog.Handler      { return s }
func TestAuthorityEventsOnlyAfterCommittedTransitions(t *testing.T) {
	// Tests in this package are sequential; no reporter survives this fixture.
	diagnostics.Default = diagnostics.Recorder{}
	db, store, c, _, policy := recoveryPGFixture(t)
	ctx := diagnostics.WithTraceParent(context.Background(), "00-11111111111111111111111111111111-2222222222222222-00")
	if _, err := store.SetBroadcastRange(ctx, c.ID, policy); err != nil {
		t.Fatal(err)
	} // replay: no second activation
	sentinel := errors.New("rollback")
	if _, err := store.UpdateCapture(ctx, c.ID, func(c *assets.RecordingCapture) error { v := 2; c.IdleEndSequenceExclusive = &v; return sentinel }); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	_, err := db.Exec(`CREATE FUNCTION reject_cutoff_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test-only commit rejection'; END $$; CREATE CONSTRAINT TRIGGER reject_cutoff AFTER UPDATE ON recording_captures DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.idle_end_sequence_exclusive=2) EXECUTE FUNCTION reject_cutoff_commit()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.UpdateCapture(ctx, c.ID, func(c *assets.RecordingCapture) error { v := 2; c.IdleEndSequenceExclusive = &v; return nil }); err == nil {
		t.Fatal("expected commit failure")
	}
	for range 2 {
		if _, err = store.UpdateCapture(ctx, c.ID, func(c *assets.RecordingCapture) error { v := 1; c.IdleEndSequenceExclusive = &v; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	policy.RangeRevision++
	policy.Revoked = true
	policy.MemberState = "blocked"
	for range 2 {
		if _, err = store.SetBroadcastRange(ctx, c.ID, policy); err != nil {
			t.Fatal(err)
		}
	}
	sink := &authorityEventSink{}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); diagnostics.Default.Run(runCtx, db.Stats, slog.New(sink)) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("reporter failed shutdown")
		}
	}()
	deadline := time.Now().Add(17 * time.Second)
	for time.Now().Before(deadline) {
		sink.mu.Lock()
		n := len(sink.events)
		sink.mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.events) != 3 {
		t.Fatalf("expected exactly activation/cutoff/revoke, got %d", len(sink.events))
	}
	counts := map[string]int{}
	for _, event := range sink.events {
		event.Attrs(func(a slog.Attr) bool {
			if a.Key == "event" {
				counts[a.Value.String()]++
			}
			if a.Key == "capture_id" {
				t.Error("raw identity")
			}
			return true
		})
	}
	for _, kind := range []string{"recovery_activated", "idle_cutoff_committed", "broadcast_revoked"} {
		if counts[kind] != 1 {
			t.Fatalf("events %v", counts)
		}
	}
}
