package main

import "testing"

func TestAudioOperationNeverFallsThroughToMigration(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		want    string
		invalid bool
	}{
		{nil, "", false}, {[]string{"--"}, "", true},
		{[]string{"--recording-audio-plan=false", "--recording-audio-apply"}, "", true}, {[]string{"--recording-audio-plan"}, "plan", false}, {[]string{"--recording-audio-apply"}, "apply", false},
		{[]string{"--recording-audio-plan", "--recording-audio-apply"}, "", true},
		{[]string{"--recording-audio-aply"}, "", true}, {[]string{"apply"}, "", true}, {[]string{"--recording-audio-apply=false"}, "", true},
	} {
		got, _, err := parseOperation(tc.args)
		if (err != nil) != tc.invalid || got != tc.want {
			t.Fatalf("%v: %q %v", tc.args, got, err)
		}
	}
}

func TestAudioApplyLimit(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		want    int
		invalid bool
	}{
		{[]string{"--recording-audio-apply"}, 100, false},
		{[]string{"--recording-audio-apply", "--recording-audio-limit=1"}, 1, false},
		{[]string{"--recording-audio-limit=28", "--recording-audio-apply"}, 28, false},
		{[]string{"--recording-audio-apply", "--recording-audio-limit=100"}, 100, false},
		{[]string{"--recording-audio-apply", "--recording-audio-limit=0"}, 0, true},
		{[]string{"--recording-audio-apply", "--recording-audio-limit=-1"}, 0, true},
		{[]string{"--recording-audio-apply", "--recording-audio-limit=101"}, 0, true},
		{[]string{"--recording-audio-apply", "--recording-audio-limit=1.5"}, 0, true},
		{[]string{"--recording-audio-plan", "--recording-audio-limit=1"}, 0, true},
		{[]string{"--recording-audio-limit=1"}, 0, true},
		{[]string{"--recording-audio-apply=false", "--recording-audio-limit=1"}, 0, true},
	} {
		_, limit, err := parseOperation(tc.args)
		if (err != nil) != tc.invalid || !tc.invalid && limit != tc.want {
			t.Fatalf("%v: limit=%d error=%v", tc.args, limit, err)
		}
	}
}
