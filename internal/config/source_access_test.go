package config

import (
	"strings"
	"testing"
)

func TestSourceAccessIsExplicitCompleteAndSecretSafe(t *testing.T) {
	for _, key := range []string{"ASSET_SOURCE_GATE_ORIGIN", "ASSET_SOURCE_GATE_TOKEN", "ASSET_ACCOUNT_ORIGIN", "ASSET_ACCOUNT_TOKEN"} {
		t.Setenv(key, "")
	}
	if settings, err := loadSourceAccess(); err != nil || settings != nil {
		t.Fatalf("default %+v %v", settings, err)
	}
	t.Setenv("ASSET_SOURCE_GATE_TOKEN", "private-test-secret")
	if _, err := loadSourceAccess(); err == nil || strings.Contains(err.Error(), "private-test-secret") {
		t.Fatalf("incomplete config %v", err)
	}
	t.Setenv("ASSET_SOURCE_GATE_ORIGIN", "https://harness.example.test")
	t.Setenv("ASSET_SOURCE_GATE_TOKEN", strings.Repeat("a", 64))
	t.Setenv("ASSET_ACCOUNT_ORIGIN", "https://account.example.test")
	t.Setenv("ASSET_ACCOUNT_TOKEN", strings.Repeat("b", 64))
	if settings, err := loadSourceAccess(); err != nil || settings == nil {
		t.Fatalf("complete %+v %v", settings, err)
	}
	t.Setenv("ASSET_ACCOUNT_TOKEN", strings.Repeat("a", 64))
	if _, err := loadSourceAccess(); err == nil {
		t.Fatal("reused credential accepted")
	}
}
