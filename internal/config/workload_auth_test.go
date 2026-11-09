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

	valid := `{"issuer":"https://issuer.example/tenant","audience":"asset-fixture","jwksUrl":"https://issuer.example/keys","principalClaim":"oid","principals":{"cms-id":"hhc-web-api","reader-id":"api-gateway","extractor-id":"hhc-web-extractor","watermark-id":"hhc-web-bulletin-worker"},"disableLegacyCallers":["hhc-web-api"]}`
	for _, test := range []struct {
		name, raw string
		valid     bool
	}{
		{"synthetic", valid, true},
		{"owner content CMS principal", strings.TrimSuffix(valid, "}") + `,"ownerContentPrincipals":["cms-id"]}`, true},
		{"owner content extractor denied", strings.TrimSuffix(valid, "}") + `,"ownerContentPrincipals":["extractor-id"]}`, false},
		{"owner content unknown principal", strings.TrimSuffix(valid, "}") + `,"ownerContentPrincipals":["unknown-id"]}`, false},

		{"trusted derivation watermark principal", strings.TrimSuffix(valid, "}") + `,"trustedDerivationPrincipals":["watermark-id"]}`, true},
		{"trusted derivation CMS denied", strings.TrimSuffix(valid, "}") + `,"trustedDerivationPrincipals":["cms-id"]}`, false}, {"invalid claim", strings.Replace(valid, `"oid"`, `"appid"`, 1), false},
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
				if err != nil || cfg.WorkloadAuth == nil || cfg.WorkloadAuth.Principals["cms-id"] != "hhc-web-api" ||
					cfg.WorkloadAuth.Principals["extractor-id"] != BulletinExtractorCaller {
					t.Fatalf("synthetic configuration rejected: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "ASSET_WORKLOAD_AUTH") || strings.Contains(err.Error(), "sensitive fixture") {
				t.Fatalf("malformed configuration yielded %v", err)
			}
		})
	}
}

func TestUploadProfileScopeConfiguration(t *testing.T) {
	cfg := Config{AllowedCallers: map[string]bool{"hhc-web-api": true, "account-api": true}}
	const base = `{"issuer":"https://issuer.example/tenant","audience":"asset-fixture","jwksUrl":"https://issuer.example/keys","principalClaim":"oid","principals":{"cms-id":"hhc-web-api","account-id":"account-api"},"uploadProfileScopes":`
	for _, test := range []struct {
		name, scope string
		valid       bool
	}{
		{"CMS approved family", `{"cms-id":[{"namespaceFamily":"cms.page","securityProfile":"web-image-v1","processingProfile":"image-variants-v1"}]}`, true},
		{"Account exact scope", `{"account-id":[{"namespaceFamily":"account.avatar","securityProfile":"avatar-jpeg-v1","processingProfile":"no-processing-v1"}]}`, true},
		{"unmapped Account scope", `{"account-id":[{"namespaceFamily":"account.new","securityProfile":"avatar-jpeg-v1","processingProfile":"no-processing-v1"}]}`, false},
		{"wrong owner", `{"account-id":[{"namespaceFamily":"cms.page","securityProfile":"web-image-v1","processingProfile":"image-variants-v1"}]}`, false},
		{"unknown principal", `{"missing-id":[{"namespaceFamily":"cms.page","securityProfile":"web-image-v1","processingProfile":"image-variants-v1"}]}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadWorkloadAuth(base+test.scope+"}", cfg)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v error=%v", test.valid, err)
			}
		})
	}
}

func TestHomeBannerDimensionDelegationDefaultsOff(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ASSET_ALLOW_DEV_CALLER_HEADER", "true")
	for _, value := range []string{"", "false", "true", "1"} {
		t.Setenv("ASSET_HOME_BANNER_DIMENSIONS_DELEGATED", value)
		cfg, err := Load()
		if (err != nil) != (value == "1") || cfg.DelegateHomeBannerSize != (value == "true") {
			t.Fatalf("value=%q delegated=%v err=%v", value, cfg.DelegateHomeBannerSize, err)
		}
	}
}
