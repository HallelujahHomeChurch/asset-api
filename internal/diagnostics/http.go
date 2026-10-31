package diagnostics

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"
)

type httpCorrelation struct{ id, parent string }
type httpCorrelationKey struct{}

var httpRequestID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// HTTPContext never changes caller-supplied audit provenance in the request headers.
func HTTPContext(ctx context.Context, headers http.Header) (context.Context, string) {
	id := headers.Get("X-HHC-Request-ID")
	if len(headers.Values("X-HHC-Request-ID")) != 1 || !httpRequestID.MatchString(id) {
		var value [16]byte
		if _, err := rand.Read(value[:]); err != nil {
			panic("request ID entropy unavailable")
		}
		id = hex.EncodeToString(value[:])
	}
	parent := headers.Get("traceparent")
	if len(headers.Values("traceparent")) != 1 || !validHTTPTrace(parent) {
		parent = ""
	}
	ctx = WithTraceParent(ctx, parent)
	return context.WithValue(ctx, httpCorrelationKey{}, httpCorrelation{id, parent}), id
}

func validHTTPTrace(value string) bool {
	if len(value) != 55 || (value[53:] != "00" && value[53:] != "01") {
		return false
	}
	ctx := WithTraceParent(context.Background(), value)
	c, _ := ctx.Value(correlationKey{}).(correlation)
	return c.trace != ""
}

// ForwardHTTP carries diagnostics only; it never creates domain audit fields.
func ForwardHTTP(ctx context.Context, headers http.Header) {
	c, ok := ctx.Value(httpCorrelationKey{}).(httpCorrelation)
	if !ok {
		return
	}
	headers.Set("X-HHC-Request-ID", c.id)
	if c.parent != "" {
		headers.Set("traceparent", c.parent)
	}
}
