package r2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"hhc/asset-api/internal/telemetry"
)

const fixturePolicy = `{"recordingId":"11111111-1111-4111-8111-111111111111","epoch":1,"rangeRevision":1,"startSequence":0,"endSequenceExclusive":null,"revoked":false,"memberState":"live"}`

func TestAuthorityReadDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		stage, state string
		cancel       bool
	}{
		{"get", "deadline", false}, {"body", "deadline", false}, {"get", "canceled", true}, {"validate", "active", false},
	} {
		t.Run(tc.stage+"_"+tc.state, func(t *testing.T) {
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(prev)
			var sends atomic.Int32
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sends.Add(1)
				if tc.cancel {
					cancel()
				}
				switch tc.stage {
				case "validate":
					io.WriteString(w, "invalid-private-fixture")
					return
				case "body":
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			store := packageListStore(server)
			id := strings.Repeat("b", 32)
			request := httptest.NewRequestWithContext(ctx, "POST", "/priv/recording-captures/"+id+"/grant", nil)
			request.Header.Set("X-HHC-Request-ID", strings.Repeat("c", 32))
			telemetry.Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := store.ReadBroadcastRange(r.Context(), id); err == nil {
					t.Error("expected failure")
				}
				w.WriteHeader(503)
			})).ServeHTTP(httptest.NewRecorder(), request)
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var e map[string]any
				if err := json.Unmarshal([]byte(line), &e); err != nil {
					t.Fatal(err)
				}
				if e["msg"] != "broadcast_authority_read" {
					continue
				}
				if e["route_id"] != "recording_live_grant" || e["request_id"] != strings.Repeat("c", 32) || e["context_state"] != tc.state || e["stage"] != tc.stage || e["sends"] != float64(sends.Load()) || sends.Load() != 1 {
					t.Fatalf("unexpected event %s", line)
				}
				if !strings.Contains(line, `"headers_or_error_ms"`) || strings.Contains(line, id) || strings.Contains(line, "invalid-private-fixture") {
					t.Fatalf("unsafe/incomplete event %s", line)
				}
				return
			}
			t.Fatal("no authority diagnostic")
		})
	}
}

// The candidate runs only in this comparison. The production client retains its SDK policy.
func TestAuthorityReadBudgetComparison(t *testing.T) {
	for _, tc := range []struct {
		name               string
		first, second      time.Duration
		candidate, success bool
		sends              int32
	}{
		{"baseline_slow_success", 2500 * time.Millisecond, 0, false, true, 1},
		{"candidate_stall_then_fast", 5 * time.Second, 0, true, true, 2},
		{"candidate_two_stalls", 5 * time.Second, 5 * time.Second, true, false, 2},
		{"candidate_two_slow_successes", 2500 * time.Millisecond, 2500 * time.Millisecond, true, false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var sends atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				delay := tc.first
				if sends.Add(1) > 1 {
					delay = tc.second
				}
				select {
				case <-r.Context().Done():
					return
				case <-time.After(delay):
					io.WriteString(w, fixturePolicy)
				}
			}))
			defer server.Close()
			store := packageListStore(server) // SDK MaxAttempts=1; only the candidate controls retries.
			parent, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			read := func(ctx context.Context) error {
				_, err := store.ReadBroadcastRange(ctx, strings.Repeat("a", 32))
				return err
			}
			var err error
			if tc.candidate {
				for range 2 {
					attempt, stop := context.WithTimeout(parent, 2*time.Second)
					err = read(attempt)
					stop()
					if !errors.Is(err, context.DeadlineExceeded) || parent.Err() != nil {
						break
					}
				}
			} else {
				err = read(parent)
			}
			if (err == nil) != tc.success || sends.Load() != tc.sends {
				t.Fatalf("success=%t, sends=%d", err == nil, sends.Load())
			}
		})
	}
}

func TestAuthorityReadReportsSDKRetry(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(prev)
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sends.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, "<Error><Code>ServiceUnavailable</Code></Error>")
			return
		}
		// Exercise a slow successful logical read, including both wire attempts.
		time.Sleep(time.Second)
		io.WriteString(w, fixturePolicy)
	}))
	defer server.Close()
	store := packageListStore(server)
	opts := store.client.Options()
	opts.RetryMaxAttempts = 2
	opts.Retryer = retry.NewStandard(func(o *retry.StandardOptions) { o.MaxAttempts = 2 })
	store.client = s3.New(opts)
	_, err := store.ReadBroadcastRange(t.Context(), strings.Repeat("a", 32))
	if err != nil || sends.Load() != 2 {
		t.Fatalf("sends=%d err=%v", sends.Load(), err)
	}
	var event struct {
		Sends    int
		Attempts map[string]struct {
			Status    int
			FirstByte int64 `json:"first_byte_ms"`
		}
	}
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &event); err != nil {
		t.Fatal(err)
	}
	if event.Sends != 2 || event.Attempts["1"].Status != 503 || event.Attempts["2"].Status != 200 || event.Attempts["2"].FirstByte < 900 {
		t.Fatalf("attempts %+v", event)
	}
}
