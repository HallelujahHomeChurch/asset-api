package dependencyhealth

import (
	"testing"
	"time"
)

func TestObservationTransitionsAndHeartbeat(t *testing.T) {
	var db, dapr observation
	now := time.Unix(1000, 0)
	for _, step := range []struct {
		after   time.Duration
		outcome string
		want    bool
	}{
		{0, "ok", true}, {30 * time.Second, "ok", false}, {time.Minute, "unavailable", true},
		{2 * time.Minute, "unavailable", false}, {6 * time.Minute, "unavailable", true},
		{6*time.Minute + time.Second, "ok", true}, {7 * time.Minute, "ok", false},
	} {
		if got := db.due(step.outcome, now.Add(step.after)); got != step.want {
			t.Fatalf("%+v: got %v", step, got)
		}
	}
	if !dapr.due("ok", now) {
		t.Fatal("DB observations suppressed initial Dapr event")
	}
}
