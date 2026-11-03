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
		got, err := parseOperation(tc.args)
		if (err != nil) != tc.invalid || got != tc.want {
			t.Fatalf("%v: %q %v", tc.args, got, err)
		}
	}
}
