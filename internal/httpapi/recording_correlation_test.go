package httpapi

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"hhc/asset-api/internal/diagnostics"
)

type correlationSink struct{ events chan slog.Record }

func (s correlationSink) Enabled(context.Context, slog.Level) bool { return true }
func (s correlationSink) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "recording_authority_committed" {
		s.events <- r.Clone()
	}
	return nil
}
func (s correlationSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s correlationSink) WithGroup(string) slog.Handler      { return s }
func TestUnsampledHTTPParentReachesBoundedLocalSink(t *testing.T) {
	diagnostics.Default = diagnostics.Recorder{}
	server := httptest.NewServer(requestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		diagnostics.Default.Transition(r.Context(), diagnostics.IdleCutoffCommitted, "0123456789abcdef0123456789abcdef", 1, 0, 170, time.Time{})
		w.WriteHeader(http.StatusNoContent)
	})))
	defer server.Close()
	for _, parent := range []string{"00-11111111111111111111111111111111-2222222222222222-00", "malformed-secret"} {
		req, err := http.NewRequest(http.MethodGet, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("traceparent", parent)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatal(resp.StatusCode)
		}
	}
	sink := correlationSink{make(chan slog.Record, 2)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		diagnostics.Default.Run(ctx, func() sql.DBStats { return sql.DBStats{} }, slog.New(sink))
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("shutdown")
		}
	}()
	for i := 0; i < 2; i++ {
		select {
		case rec := <-sink.events:
			var got any
			rec.Attrs(func(a slog.Attr) bool {
				if a.Key == "trace_id" {
					got = a.Value.Any()
				}
				return true
			})
			if i == 0 && got != "11111111111111111111111111111111" {
				t.Fatalf("unsampled parent lost %v", got)
			}
			if i == 1 && got != nil {
				t.Fatal("malformed trace retained")
			}
		case <-time.After(17 * time.Second):
			t.Fatal("no local sink arrival")
		}
	}
}
