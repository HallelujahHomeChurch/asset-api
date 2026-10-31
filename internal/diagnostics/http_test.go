package diagnostics

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestHTTPContextAndForwarding(t *testing.T) {
	const trace = "00-11111111111111111111111111111111-2222222222222222-01"
	for _, values := range [][]string{nil, {"request-1"}, {"bad value"}, {strings.Repeat("x", 129)}, {"one", "two"}} {
		h := http.Header{"X-Hhc-Request-Id": values, "Traceparent": {trace}}
		ctx, id := HTTPContext(context.Background(), h)
		if !httpRequestID.MatchString(id) {
			t.Fatal("invalid normalized id")
		}
		if len(values) == 1 && values[0] == "request-1" && id != "request-1" {
			t.Fatal("valid ID changed")
		}
		if len(h.Values("X-HHC-Request-ID")) != len(values) {
			t.Fatal("audit provenance changed")
		}
		out := http.Header{}
		ForwardHTTP(ctx, out)
		if out.Get("X-HHC-Request-ID") != id || out.Get("traceparent") != trace {
			t.Fatal("correlation lost")
		}
	}
	for _, values := range [][]string{{"bad"}, {strings.TrimSuffix(trace, "01") + "ff"}, {trace, trace}, {"00-00000000000000000000000000000000-2222222222222222-01"}} {
		ctx, _ := HTTPContext(context.Background(), http.Header{"Traceparent": values})
		out := http.Header{}
		ForwardHTTP(ctx, out)
		if out.Get("traceparent") != "" {
			t.Fatal("invalid trace propagated")
		}
	}
}
