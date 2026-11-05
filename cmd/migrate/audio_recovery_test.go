package main

import (
	"strings"
	"testing"
)

func TestAudioRecoveryRequiresExactPlanOrFencedApply(t *testing.T) {
	id := strings.Repeat("a", 32)
	claim := strings.Repeat("b", 32)
	for _, tc := range []struct {
		args             []string
		handled, invalid bool
	}{
		{nil, false, false}, {[]string{"--recording-audio-plan"}, false, false},
		{[]string{"--recording-audio-retry-plan=" + id}, true, false},
		{[]string{"--recording-audio-retry-apply=" + id, "--recording-audio-last-claim=" + claim}, true, false},
		{[]string{"--recording-audio-retry-apply=" + id}, true, true},
		{[]string{"--recording-audio-retry-plan=" + id, "--recording-audio-last-claim=" + claim}, true, true},
		{[]string{"--recording-audio-retry-plan=" + id, "--recording-audio-retry-apply=" + id}, true, true},
		{[]string{"--recording-audio-retry-plan=" + id, "--recording-audio-apply"}, true, true},
		{[]string{"--recording-audio-retry-plan="}, true, true},
		{[]string{"--recording-audio-last-claim=" + claim}, true, true},
		{[]string{"--recording-audio-retry-plan=bad"}, true, true},
		{[]string{"--recording-audio-retry-plan=" + id, "extra"}, true, true},
	} {
		_, handled, err := parseAudioRecovery(tc.args)
		if handled != tc.handled || (err != nil) != tc.invalid {
			t.Fatalf("%v handled=%v err=%v", tc.args, handled, err)
		}
	}
}
