package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDatabaseFailuresKeepRequestContext(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		for _, state := range []string{"active", "canceled", "deadline"} {
			t.Run(format+"/"+state, func(t *testing.T) {
				var logs bytes.Buffer
				var handler slog.Handler = slog.NewJSONHandler(&logs, nil)
				if format == "text" {
					handler = slog.NewTextHandler(&logs, nil)
				}
				old := slog.Default()
				slog.SetDefault(slog.New(handler))
				defer slog.SetDefault(old)
				request := httptest.NewRequest("GET", "/priv/recording-packages/private-package/ready?recordingId=private-recording&token=secret", nil)
				request.Header.Set("X-HHC-Request-ID", "0123456789abcdef0123456789abcdef")
				Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					ctx := r.Context()
					switch state {
					case "canceled":
						var cancel context.CancelFunc
						ctx, cancel = context.WithCancel(ctx)
						cancel()
					case "deadline":
						var cancel context.CancelFunc
						ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
						defer cancel()
					}
					RecordDatabaseOutcome(ctx, "db_query_lifecycle", time.Millisecond, true)
					w.WriteHeader(http.StatusServiceUnavailable)
				})).ServeHTTP(httptest.NewRecorder(), request)
				lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
				if len(lines) != 2 {
					t.Fatalf("want one child and one summary, got %d", len(lines))
				}
				for i, line := range lines {
					if format == "json" {
						var event map[string]any
						if err := json.Unmarshal([]byte(line), &event); err != nil {
							t.Fatal(err)
						}
						if event["request_id"] != "0123456789abcdef0123456789abcdef" || event["route_id"] != "recording_package_ready" {
							t.Fatalf("lost request correlation: %s", line)
						}
						if i == 0 && event["context_state"] != state {
							t.Fatalf("lost context state: %s", line)
						}
					} else {
						for _, want := range []string{"request_id=0123456789abcdef0123456789abcdef", "route_id=recording_package_ready"} {
							if !strings.Contains(line, want) {
								t.Fatalf("missing %s: %s", want, line)
							}
						}
						if i == 0 && !strings.Contains(line, "context_state="+state) {
							t.Fatalf("lost context state: %s", line)
						}
					}
				}
				for _, private := range []string{"private-package", "private-recording", "secret"} {
					if strings.Contains(logs.String(), private) {
						t.Fatal("private request data leaked")
					}
				}
			})
		}
	}
}

func TestRecordingReadDiagnosticClasses(t *testing.T) {
	for _, tc := range []struct{ method, path, want string }{
		{"GET", "/priv/recording-packages/private-package", "recording_package_read"},
		{"HEAD", "/priv/recording-packages/private-package/ready", "recording_package_ready"},
		{"POST", "/priv/recording-packages/private-package/ready", "other"},
		{"GET", "/priv/recording-packages/private-package/unknown", "other"},
		{"GET", "/priv/recording-packages//ready", "other"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			logs := captureDiagnosticLogs(t)
			Server(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, tc.path, nil))
			var event map[string]any
			if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			if event["route_id"] != tc.want {
				t.Fatalf("want %s, got %v", tc.want, event["route_id"])
			}
		})
	}
}
