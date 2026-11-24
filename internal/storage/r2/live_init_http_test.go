package r2

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLiveInitHeaderRecovery(t *testing.T) {
	for _, scenario := range []string{"recover", "slow_success", "slow_second_headers", "slow_body", "parent_cancel", "both_stall", "missing", "ordinary_segment"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			var sends atomic.Int32
			parentBudget := 9 * time.Second
			if scenario == "slow_success" || scenario == "recover" {
				parentBudget = 20 * time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), parentBudget)
			defer cancel()
			const budget = 200 * time.Millisecond
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := sends.Add(1)
				switch {
				case scenario == "missing":
					w.WriteHeader(404)
					io.WriteString(w, "<Error><Code>NoSuchKey</Code></Error>")
					return
				case scenario == "parent_cancel":
					cancel()
					<-r.Context().Done()
					return
				case scenario == "both_stall" || n == 1 && (scenario == "recover" || scenario == "slow_second_headers"):
					<-r.Context().Done()
					return
				case scenario == "slow_body":
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					time.Sleep(2 * budget)
				case scenario == "slow_second_headers" || scenario == "ordinary_segment":
					time.Sleep(2 * budget)
				case scenario == "slow_success":
					time.Sleep(2500 * time.Millisecond)
				}
				io.WriteString(w, "verified by caller")
			}))
			defer server.Close()
			store := packageListStore(server)
			const key = "recordings/captures/fixture/final/720p/init.mp4"
			var body io.ReadCloser
			var err error
			switch scenario {
			case "slow_success", "recover":
				body, err = store.OpenLiveInit(ctx, key)
			case "ordinary_segment":
				body, err = store.Open(ctx, strings.Replace(key, "init.mp4", "seg-000449.m4s", 1))
			default:
				body, err = store.open(ctx, key, budget)
			}
			expected := int32(1)
			switch scenario {
			case "recover", "slow_second_headers", "both_stall":
				expected = 2
			}
			if sends.Load() != expected {
				t.Fatalf("sent %d GETs, want %d; err=%v", sends.Load(), expected, err)
			}
			switch scenario {
			case "missing":
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("expected missing object; got %v", err)
				}
			case "both_stall", "parent_cancel":
				if err == nil || ctx.Err() == nil {
					t.Fatalf("expected parent cancellation; got %v", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				defer body.Close()
				data, err := io.ReadAll(body)
				if err != nil || string(data) != "verified by caller" {
					t.Fatalf("body=%q err=%v", data, err)
				}
			}
		})
	}
}
