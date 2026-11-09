package config

import (
	"strings"
	"testing"
)

func TestWorkloadAuthRuntimeConfig(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ASSET_ALLOW_DEV_CALLER_HEADER", "true")
	t.Setenv("ASSET_WORKLOAD_AUTH", "")
	cfg, err := Load()
	if err != nil || cfg.WorkloadAuth != nil {
		t.Fatalf("default workload verifier config is not disabled: %v", err)
	}

	valid := `{"issuer":"https://issuer.example/tenant","audience":"asset-fixture","jwksUrl":"https://issuer.example/keys","principalClaim":"oid","principals":{"cms-id":"hhc-web-api","reader-id":"api-gateway"},"disableLegacyCallers":["hhc-web-api"]}`
	for _, test := range []struct {
		name, raw string
		valid     bool
	}{
		{"synthetic", valid, true},
		{"invalid claim", strings.Replace(valid, `"oid"`, `"appid"`, 1), false},
		{"missing audience", strings.Replace(valid, `"asset-fixture"`, `""`, 1), false},
		{"unsafe JWKS URL", strings.Replace(valid, "https://issuer.example/keys", "http://issuer.example/keys", 1), false},
		{"principal cannot add a caller", strings.Replace(valid, `"reader-id":"api-gateway"`, `"reader-id":"unknown-caller"`, 1), false},
		{"unknown legacy alias", strings.Replace(valid, `["hhc-web-api"]`, `["unknown-caller"]`, 1), false},
		{"unknown option", strings.TrimSuffix(valid, "}") + `,"secret":"sensitive fixture"}`, false},
		{"trailing JSON", valid + ` {}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("ASSET_WORKLOAD_AUTH", test.raw)
			cfg, err := Load()
			if test.valid {
				if err != nil || cfg.WorkloadAuth == nil || cfg.WorkloadAuth.Principals["cms-id"] != "hhc-web-api" {
					t.Fatalf("synthetic configuration rejected: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "ASSET_WORKLOAD_AUTH") || strings.Contains(err.Error(), "sensitive fixture") {
				t.Fatalf("malformed configuration yielded %v", err)
			}
		})
	}
}
