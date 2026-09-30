package logging

import (
	"context"
	"log/slog"
	"testing"
)

func TestLevel(t *testing.T) {
	old := slog.Default()
	defer slog.SetDefault(old)
	for _, tc := range []struct {
		value string
		level slog.Level
	}{{"", slog.LevelInfo}, {"debug", slog.LevelDebug}, {"info", slog.LevelInfo}, {"warn", slog.LevelWarn}, {"error", slog.LevelError}} {
		t.Setenv("LOG_LEVEL", tc.value)
		if err := Init(); err != nil {
			t.Fatal(err)
		}
		if !slog.Default().Enabled(context.Background(), tc.level) || slog.Default().Enabled(context.Background(), tc.level-1) {
			t.Fatal(tc.value)
		}
	}
	t.Setenv("LOG_LEVEL", "invalid")
	if Init() == nil {
		t.Fatal("invalid setting accepted")
	}
}
