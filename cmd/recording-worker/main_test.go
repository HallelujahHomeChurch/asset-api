package main

import (
	"context"
	"testing"
)

func TestDisabledHLSDoesNotStartLegacyProcessing(t *testing.T) {
	t.Setenv("ASSET_RECORDING_HLS_ENABLED", "false")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("ASSET_R2_ACCESS_KEY_ID", "")
	if err := run(context.Background()); err != nil {
		t.Fatalf("disabled worker accessed runtime dependencies: %v", err)
	}
}
