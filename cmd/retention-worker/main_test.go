package main

import "testing"

func TestApplyEnabledDefaultsOff(t *testing.T) {
	for _, value := range []string{"", "false"} {
		enabled, err := applyEnabled(value)
		if err != nil || enabled {
			t.Fatalf("value=%q enabled=%v err=%v", value, enabled, err)
		}
	}
	if enabled, err := applyEnabled("true"); err != nil || !enabled {
		t.Fatalf("enabled=%v err=%v", enabled, err)
	}
	if _, err := applyEnabled("invalid"); err == nil {
		t.Fatal("invalid value accepted")
	}
}
