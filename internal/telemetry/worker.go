package telemetry

import (
	"context"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"time"
)

// StartWorker uses the same managed collector and bounded root sampler as HTTP.
// Finish must run before the command exits, including on failure/cancellation.
func StartWorker(ctx context.Context, kind string) (context.Context, func(error)) {
	switch kind {
	case "scan", "derivative", "recording", "retention", "signature":
	default:
		kind = "job"
	}
	shutdown := Configure(ctx)
	ctx, span := otel.Tracer("asset-api/worker").Start(ctx, "worker."+kind, trace.WithNewRoot())
	started := time.Now()
	return ctx, func(err error) {
		if err != nil {
			span.SetStatus(codes.Error, "")
		}
		RecordOutcome(ctx, "worker", 0, time.Since(started), err != nil)
		span.End()
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if shutdown(stopCtx) != nil {
			RecordOutcome(ctx, "worker", 0, 0, true)
		}
	}
}
