package config

import "testing"

func TestRecordingEventsDisabledAndRequiresAllQueues(t *testing.T) {
	t.Setenv("ASSET_RECORDING_EVENTS_ENABLED", "false")
	cfg, err := RecordingEvents()
	if err != nil || cfg.Enabled {
		t.Fatal(cfg, err)
	}
	t.Setenv("ASSET_RECORDING_EVENTS_ENABLED", "true")
	if _, err = RecordingEvents(); err == nil {
		t.Fatal("missing queues allowed")
	}
	for _, lane := range []string{"LIVE", "PROCESSING", "PREVIEW"} {
		t.Setenv("ASSET_RECORDING_"+lane+"_QUEUE_URL", "https://example.queue.core.windows.net/recording")
		t.Setenv("ASSET_RECORDING_"+lane+"_POISON_URL", "https://example.queue.core.windows.net/poison")
	}
	if _, err = RecordingEvents(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSET_RECORDING_LIVE_QUEUE_URL", "http://evil.example/queue?secret=bad")
	if _, err = RecordingEvents(); err == nil {
		t.Fatal("invalid queue destination allowed")
	}
}
func TestWorkerModeAllowlist(t *testing.T) {
	for _, v := range []string{"legacy", "live", "processing", "preview", "maintenance"} {
		t.Setenv("ASSET_RECORDING_WORKER_MODE", v)
		if _, err := RecordingWorkerMode(); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ASSET_RECORDING_WORKER_MODE", "arbitrary")
	if _, err := RecordingWorkerMode(); err == nil {
		t.Fatal("unbounded mode")
	}
}
