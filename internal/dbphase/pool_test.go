package dbphase

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"strings"
	"testing"
	"time"
)

func TestQueryTracerKeepsOnlyTiming(t *testing.T) {
	var phase string
	var elapsed time.Duration
	var failed bool
	tracer := queryTracer{report: func(_ context.Context, p string, d time.Duration, f bool) { phase, elapsed, failed = p, d, f }}
	ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "secret SQL", Args: []any{"secret argument"}})
	if _, ok := ctx.Value(queryKey{}).(time.Time); !ok {
		t.Fatal("timing missing")
	}
	ctx = context.WithValue(ctx, queryKey{}, time.Now().Add(-300*time.Millisecond))
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: errors.New("secret error")})
	if phase != "db_query_lifecycle" || elapsed < 300*time.Millisecond || !failed {
		t.Fatal("incorrect lifecycle outcome")
	}
}
func TestInvalidConfigurationIsSanitized(t *testing.T) {
	_, err := OpenURL("postgres://secret:password@invalid%host", func(context.Context, string, time.Duration, bool) {})
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "password") {
		t.Fatal("unsafe configuration failure")
	}
}
