package main

import (
	"hhc/asset-api/internal/config"
	"testing"
)

func TestExtractorWorkloadUsesExistingWebsiteOwner(t *testing.T) {
	auth := workloadAuth(config.Config{WorkloadTenantID: "tenant", WorkloadIssuer: "issuer", WorkloadAudience: "audience", WorkloadRequiredRole: "Asset.Invoke", LineWorkloadClientID: "line-client", LineWorkloadObjectID: "line-object", ExtractorWorkloadClientID: "extractor-client", ExtractorWorkloadObjectID: "extractor-object"})
	if auth.Callers["extractor-client"].Service != "hhc-web-api" || auth.Callers["extractor-client"].ObjectID != "extractor-object" {
		t.Fatal("extractor must be the existing hhc-web-api asset owner")
	}
	if auth.Callers["line-client"].Service != "hhc-line-function-bot" || auth.RequiredRole != "Asset.Invoke" || len(auth.Callers) != 2 {
		t.Fatal("existing LINE mapping or role changed")
	}
}

func TestLINEWorkloadKeepsSeparateExactIdentities(t *testing.T) {
	auth := workloadAuth(config.Config{
		LineWorkloadClientID: "worker-client", LineWorkloadObjectID: "worker-object",
		LineAPIWorkloadClientID: "api-client", LineAPIWorkloadObjectID: "api-object",
	})
	for client, object := range map[string]string{"worker-client": "worker-object", "api-client": "api-object"} {
		caller := auth.Callers[client]
		if caller.Service != "hhc-line-function-bot" || caller.ObjectID != object {
			t.Fatalf("unexpected LINE workload identity for %s", client)
		}
	}
}
