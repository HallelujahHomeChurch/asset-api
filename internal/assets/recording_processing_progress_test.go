package assets

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestProcessingProgressValidation(t *testing.T) {
	now := time.Date(2026, 10, 6, 14, 0, 0, 0, time.UTC)
	zero, one := int64(0), int64(1)
	valid := RecordingProcessingProgress{Attempt: 1, Phase: "package_validation", Rendition: "720p", ObjectsVerified: &zero, ObjectsTotal: &one, AttemptStartedAt: now, PhaseStartedAt: now, LastProgressAt: now, HeartbeatAt: now}
	if err := ValidateRecordingProcessingProgress(valid); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(valid)
	if err != nil || !strings.Contains(string(data), `"objectsVerified":0`) {
		t.Fatalf("zero progress omitted: %s %v", data, err)
	}
	for name, mutate := range map[string]func(*RecordingProcessingProgress){
		"unknown phase":               func(p *RecordingProcessingProgress) { p.Phase = "ready" },
		"unknown rendition":           func(p *RecordingProcessingProgress) { p.Rendition = "4k" },
		"negative attempt":            func(p *RecordingProcessingProgress) { p.Attempt = -1 },
		"exhausted attempt":           func(p *RecordingProcessingProgress) { p.Attempt = 4 },
		"zero claimed attempt":        func(p *RecordingProcessingProgress) { p.Attempt = 0 },
		"negative count":              func(p *RecordingProcessingProgress) { n := int64(-1); p.BytesVerified = &n },
		"done exceeds total":          func(p *RecordingProcessingProgress) { n := int64(2); p.ObjectsVerified = &n },
		"phase predates attempt":      func(p *RecordingProcessingProgress) { p.PhaseStartedAt = now.Add(-time.Second) },
		"progress predates phase":     func(p *RecordingProcessingProgress) { p.LastProgressAt = now.Add(-time.Second) },
		"heartbeat predates progress": func(p *RecordingProcessingProgress) { p.HeartbeatAt = now.Add(-time.Second) },
		"missing clock":               func(p *RecordingProcessingProgress) { p.AttemptStartedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			p := valid
			mutate(&p)
			if !errors.Is(ValidateRecordingProcessingProgress(p), ErrInvalidInput) {
				t.Fatalf("invalid summary accepted: %+v", p)
			}
		})
	}
	queued := RecordingProcessingProgress{Phase: "queued", AttemptStartedAt: now, PhaseStartedAt: now, LastProgressAt: now, HeartbeatAt: now}
	if err := ValidateRecordingProcessingProgress(queued); err != nil {
		t.Fatal(err)
	}
}
