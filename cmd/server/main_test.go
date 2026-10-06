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
