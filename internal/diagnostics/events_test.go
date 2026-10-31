package diagnostics

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

const fixtureCapture = "0123456789abcdef0123456789abcdef"
const fixtureTrace = "00-11111111111111111111111111111111-2222222222222222-00"

func TestTransitionCorrelationPrivacyAndByteBudget(t *testing.T) {
	for _, parent := range []string{fixtureTrace, strings.TrimSuffix(fixtureTrace, "00") + "01", "https://token.invalid/?secret=PRIVATE", strings.Repeat("a", 4096), "00-00000000000000000000000000000000-2222222222222222-00"} {
		var r Recorder
		r.Transition(WithTraceParent(context.Background(), parent), IdleCutoffCommitted, fixtureCapture, math.MaxInt64, math.MaxInt64, math.MaxInt, time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC))
		var b bytes.Buffer
		r.flushEvents(context.Background(), slog.New(slog.NewJSONHandler(&b, nil)))
		if b.Len() > 1024 {
			t.Fatalf("byte budget: %d", b.Len())
		}
		var out map[string]any
		if err := json.Unmarshal(b.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(b.String(), fixtureCapture) || strings.Contains(b.String(), "PRIVATE") {
			t.Fatal("sensitive raw input")
		}
		want := sha256.Sum256([]byte("hhc.recording.capture.v1\x00" + fixtureCapture))
		if out["capture_ref"] != hex.EncodeToString(want[:]) {
			t.Fatal("canonical32hex hash mismatch")
		}
		if len(parent) == 55 && parent[3:35] == strings.Repeat("1", 32) {
			if out["trace_id"] != strings.Repeat("1", 32) {
				t.Fatal("unsampled trace lost")
			}
		} else if out["trace_id"] != nil {
			t.Fatal("invalid parent retained")
		}
		t.Logf("event bytes %d", b.Len())
	}
}
func TestTransitionQueueBoundAndConcurrentEnqueue(t *testing.T) {
	var r Recorder
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() { r.Transition(context.Background(), RecoveryActivated, fixtureCapture, 1, 2, -1, time.Time{}) })
	}
	wg.Wait()
	if r.eventCount != 32 || r.eventSnapshot()[1] != 68 {
		t.Fatal("queue unbounded/lost drop count")
	}
	var b bytes.Buffer
	r.flushEvents(context.Background(), slog.New(slog.NewJSONHandler(&b, nil)))
	if strings.Count(b.String(), "recording_authority_committed") != 16 || r.eventCount != 16 {
		t.Fatal("per-tick bound")
	}
	if r.eventSnapshot()[0] != 16 {
		t.Fatal("written count")
	}
}

type failingEventHandler struct{}

func (failingEventHandler) Enabled(context.Context, slog.Level) bool { return true }
func (failingEventHandler) Handle(context.Context, slog.Record) error {
	return errors.New("secret-provider-url")
}
func (h failingEventHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h failingEventHandler) WithGroup(string) slog.Handler      { return h }
func TestTransitionSinkFailureAndCancellation(t *testing.T) {
	var r Recorder
	r.Transition(context.Background(), BroadcastRevoked, fixtureCapture, 1, 3, 4, time.Time{})
	r.flushEvents(context.Background(), slog.New(failingEventHandler{}))
	if r.eventSnapshot()[2] != 1 {
		t.Fatal("write failure not counted")
	}
	r.Transition(context.Background(), BroadcastRevoked, fixtureCapture, 1, 3, 4, time.Time{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.flushEvents(ctx, slog.New(failingEventHandler{}))
	if r.eventSnapshot()[3] != 1 {
		t.Fatal("cancel loss not counted")
	}
}

func TestTransitionTextEncodingBoundAndFilteredSink(t *testing.T) {
	var r Recorder
	var b bytes.Buffer
	r.Transition(WithTraceParent(context.Background(), fixtureTrace), RecoveryActivated, fixtureCapture, math.MaxInt64, math.MaxInt64, math.MaxInt, time.Now())
	r.flushEvents(context.Background(), slog.New(slog.NewTextHandler(&b, nil)))
	if b.Len() > 1024 {
		t.Fatal("text bytes exceeded")
	}
	r.Transition(context.Background(), RecoveryActivated, fixtureCapture, 1, 1, -1, time.Time{})
	r.flushEvents(context.Background(), slog.New(slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelError})))
	if r.eventSnapshot()[3] != 1 {
		t.Fatal("filtered event not counted")
	}
}

func TestBlockedEventWriterDoesNotHoldQueueLock(t *testing.T) {
	var r Recorder
	r.Transition(context.Background(), RecoveryActivated, fixtureCapture, 1, 1, -1, time.Time{})
	sink := &blockedSink{make(chan struct{}, 1), make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); r.flushEvents(ctx, slog.New(slog.NewTextHandler(sink, nil))) }()
	select {
	case <-sink.entered:
	case <-time.After(time.Second):
		t.Fatal("event sink not called")
	}
	queued := make(chan struct{})
	go func() {
		r.Transition(context.Background(), BroadcastRevoked, fixtureCapture, 1, 2, -1, time.Time{})
		close(queued)
	}()
	select {
	case <-queued:
	case <-time.After(time.Second):
		t.Fatal("blocked writer held queue lock")
	}
	cancel()
	close(sink.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("writer failed to exit after release")
	}
	if r.eventSnapshot()[2] != 1 {
		t.Fatal("sink failure not accounted")
	}
}

func TestTransitionPrefersActiveSDKSpanIncludingUnsampledRoot(t *testing.T) {
	for _, sampler := range []sdktrace.Sampler{sdktrace.AlwaysSample(), sdktrace.NeverSample()} {
		provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sampler))
		ctx, span := provider.Tracer("test").Start(WithTraceParent(context.Background(), fixtureTrace), "local")
		active := trace.SpanContextFromContext(ctx)
		var recorder Recorder
		recorder.Transition(ctx, IdleCutoffCommitted, fixtureCapture, 1, 0, 0, time.Time{})
		span.End()
		_ = provider.Shutdown(context.Background())
		var output bytes.Buffer
		recorder.flushEvents(context.Background(), slog.New(slog.NewJSONHandler(&output, nil)))
		var got map[string]any
		if err := json.Unmarshal(output.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got["trace_id"] != active.TraceID().String() || got["parent_span_id"] != active.SpanID().String() {
			t.Fatal("active application span not correlated")
		}
	}
}
