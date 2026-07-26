package config

import "testing"

func TestLoadEnablesDevelopmentCallerHeaderExplicitly(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ASSET_ALLOW_DEV_CALLER_HEADER", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowDevCallerHeader {
		t.Fatal("development caller header was not enabled")
	}
	if !cfg.AllowedCallers["account-api"] {
		t.Fatal("account-api is missing from the default caller allowlist")
	}
}
