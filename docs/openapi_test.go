package docs_test

import (
	"os"
	"strings"
	"testing"
)

type documentedOperation struct {
	method, path, callers string
}

func TestOpenAPIContract(t *testing.T) {
	raw, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	document := string(raw)
	for _, operation := range []documentedOperation{
		{"GET", "/health", "[]"},
		{"GET", "/ready", "[]"},
		{"GET", "/api/assets/public/{assetID}", "[api-gateway]"},
		{"GET", "/api/assets/public/{assetID}/{variant}", "[api-gateway]"},
		{"GET", "/api/assets/collections", "[api-gateway]"},
		{"GET", "/api/assets/collections/{collectionID}/changes", "[api-gateway]"},
		{"GET", "/api/assets/collections/{collectionID}/items/{itemID}", "[api-gateway]"},
		{"POST", "/api/assets/collections/{collectionID}/items/{itemID}/content-ticket", "[api-gateway]"},
		{"GET", "/api/assets/collections/{collectionID}/items/{itemID}/content", "[api-gateway]"},
		{"GET", "/api/assets/content", "[api-gateway]"},
		{"POST", "/priv/assets/upload-sessions", "[account-api, hhc-web-api, hhc-line-function-bot]"},
		{"GET", "/priv/assets/operations", "[account-api, hhc-web-api, hhc-line-function-bot]"},
		{"GET", "/priv/assets/collections", "[hhc-line-function-bot]"},
		{"POST", "/priv/assets/collections", "[hhc-line-function-bot]"},
		{"GET", "/priv/assets/collections/{collectionID}", "[hhc-line-function-bot]"},
		{"PATCH", "/priv/assets/collections/{collectionID}", "[hhc-line-function-bot]"},
		{"DELETE", "/priv/assets/collections/{collectionID}", "[hhc-line-function-bot]"},
		{"GET", "/priv/assets/collections/{collectionID}/items", "[hhc-line-function-bot]"},
		{"POST", "/priv/assets/collections/{collectionID}/items", "[hhc-line-function-bot]"},
		{"POST", "/priv/assets/collections/{collectionID}/items/content-tickets", "[hhc-line-function-bot]"},
		{"POST", "/priv/assets/collections/{collectionID}/items/retention", "[hhc-line-function-bot]"},
		{"POST", "/priv/assets/collections/{collectionID}/items/delete", "[hhc-line-function-bot]"},
		{"PATCH", "/priv/assets/collections/{collectionID}/items/{itemID}", "[hhc-line-function-bot]"},
		{"DELETE", "/priv/assets/collections/{collectionID}/items/{itemID}", "[hhc-line-function-bot]"},
		{"PATCH", "/priv/assets/collections/{collectionID}/retention", "[hhc-line-function-bot]"},
		{"POST", "/priv/assets/collections/{collectionID}/acl", "[hhc-line-function-bot]"},
		{"DELETE", "/priv/assets/collections/{collectionID}/acl/{aclID}", "[hhc-line-function-bot]"},
		{"GET", "/priv/assets/{assetID}", "[account-api, hhc-web-api, hhc-line-function-bot]"},
		{"GET", "/priv/assets/{assetID}/download", "[account-api, hhc-web-api, hhc-line-function-bot]"},
		{"GET", "/priv/assets/{assetID}/public-url", "[account-api, hhc-web-api, hhc-line-function-bot]"},
		{"POST", "/priv/assets/{assetID}/complete", "[account-api, hhc-web-api, hhc-line-function-bot]"},
		{"POST", "/priv/assets/{assetID}/grants", "[account-api, hhc-web-api, hhc-line-function-bot]"},
		{"DELETE", "/priv/assets/{assetID}/grants/{grantID}", "[account-api, hhc-web-api, hhc-line-function-bot]"},
		{"POST", "/priv/assets/{assetID}/scan/requeue", "[account-api, hhc-web-api, hhc-line-function-bot]"},
		{"DELETE", "/priv/assets/{assetID}", "[account-api, hhc-web-api, hhc-line-function-bot]"},
	} {
		assertOperation(t, document, operation)
	}

	for _, value := range []string{"openapi: 3.1.0", "x-hhc-service: asset-api", "x-hhc-owner: HHC Platform", "x-hhc-repository: HallelujahHomeChurch/asset-api", "checksumSha256", "detectedMimeType", "short-lived upload target", "CMS business records remain owned by the caller"} {
		if !strings.Contains(document, value) {
			t.Fatalf("missing contract detail %q", value)
		}
	}
}

func assertOperation(t *testing.T, document string, operation documentedOperation) {
	t.Helper()
	path := "  " + operation.path + ":\n"
	start := strings.Index(document, path)
	if start < 0 {
		t.Fatalf("missing %s %s", operation.method, operation.path)
	}
	rest := document[start+len(path):]
	if next := strings.Index(rest, "\n  /"); next >= 0 {
		rest = rest[:next]
	}
	method := "    " + strings.ToLower(operation.method) + ":\n"
	start = strings.Index(rest, method)
	if start < 0 {
		t.Fatalf("missing %s %s", operation.method, operation.path)
	}
	rest = rest[start+len(method):]
	rest = operationBody(rest)
	if !strings.Contains(rest, "x-hhc-callers: "+operation.callers) {
		t.Fatalf("%s %s callers=%q", operation.method, operation.path, operation.callers)
	}
}

func operationBody(value string) string {
	for offset := 0; ; {
		next := strings.Index(value[offset:], "\n    ")
		if next < 0 {
			return value
		}
		next += offset
		if len(value) == next+5 || value[next+5] != ' ' {
			return value[:next]
		}
		offset = next + 1
	}
}
