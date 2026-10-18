package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type RecordingEventConfig struct {
	Enabled        bool
	Queues, Poison map[string]string
}

func RecordingEvents() (RecordingEventConfig, error) {
	c := RecordingEventConfig{Queues: map[string]string{}, Poison: map[string]string{}}
	enabled, err := strconv.ParseBool(value("ASSET_RECORDING_EVENTS_ENABLED", "false"))
	if err != nil {
		return c, fmt.Errorf("invalid ASSET_RECORDING_EVENTS_ENABLED")
	}
	c.Enabled = enabled
	if !enabled {
		return c, nil
	}
	for _, lane := range []string{"live", "processing", "preview"} {
		for _, kind := range []string{"QUEUE", "POISON"} {
			name := "ASSET_RECORDING_" + strings.ToUpper(lane) + "_" + kind + "_URL"
			raw := os.Getenv(name)
			u, e := url.Parse(raw)
			if e != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Hostname(), ".queue.core.windows.net") || u.Port() != "" || strings.Count(u.Path, "/") != 1 || len(u.Path) < 4 {
				return c, fmt.Errorf("invalid %s", name)
			}
			if kind == "QUEUE" {
				c.Queues[lane] = raw
			} else {
				c.Poison[lane] = raw
			}
		}
	}
	return c, nil
}
func RecordingWorkerMode() (string, error) {
	mode := value("ASSET_RECORDING_WORKER_MODE", "legacy")
	switch mode {
	case "legacy", "live", "processing", "preview", "maintenance":
		return mode, nil
	default:
		return "", fmt.Errorf("invalid ASSET_RECORDING_WORKER_MODE")
	}
}
