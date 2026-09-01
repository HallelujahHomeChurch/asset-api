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
		{"POST", "/api/assets/sync-receipts", "[api-gateway]"},
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

func TestOpenAPIHomeBannerContract(t *testing.T) {
	raw, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	document := string(raw)
	upload := schemaBlockFor(t, document, "CreateUploadRequest")
	for _, value := range []string{"cms.home.banner", "ownerService=hhc-web-api", "image/jpeg", "1900x700", "processingStatus=not_required", "granted original"} {
		assertContains(t, upload, value)
	}
	complete := operationBlockFor(t, document, documentedOperation{"POST", "/priv/assets/{assetID}/complete", ""})
	assertContains(t, complete, "namespace-specific detected MIME and decoded image dimensions")
}

func TestOpenAPISecurityAndWireContracts(t *testing.T) {
	raw, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	document := string(raw)

	for _, operation := range []documentedOperation{
		{"POST", "/priv/assets/upload-sessions", ""},
		{"GET", "/priv/assets/operations", ""},
		{"GET", "/priv/assets/collections", ""},
		{"POST", "/priv/assets/collections", ""},
		{"GET", "/priv/assets/collections/{collectionID}", ""},
		{"PATCH", "/priv/assets/collections/{collectionID}", ""},
		{"DELETE", "/priv/assets/collections/{collectionID}", ""},
		{"GET", "/priv/assets/collections/{collectionID}/items", ""},
		{"POST", "/priv/assets/collections/{collectionID}/items", ""},
		{"POST", "/priv/assets/collections/{collectionID}/items/content-tickets", ""},
		{"POST", "/priv/assets/collections/{collectionID}/items/retention", ""},
		{"POST", "/priv/assets/collections/{collectionID}/items/delete", ""},
		{"PATCH", "/priv/assets/collections/{collectionID}/items/{itemID}", ""},
		{"DELETE", "/priv/assets/collections/{collectionID}/items/{itemID}", ""},
		{"PATCH", "/priv/assets/collections/{collectionID}/retention", ""},
		{"POST", "/priv/assets/collections/{collectionID}/acl", ""},
		{"DELETE", "/priv/assets/collections/{collectionID}/acl/{aclID}", ""},
		{"GET", "/priv/assets/{assetID}", ""},
		{"GET", "/priv/assets/{assetID}/download", ""},
		{"GET", "/priv/assets/{assetID}/public-url", ""},
		{"POST", "/priv/assets/{assetID}/complete", ""},
		{"POST", "/priv/assets/{assetID}/grants", ""},
		{"DELETE", "/priv/assets/{assetID}/grants/{grantID}", ""},
		{"POST", "/priv/assets/{assetID}/scan/requeue", ""},
		{"DELETE", "/priv/assets/{assetID}", ""},
	} {
		assertContains(t, operationBlockFor(t, document, operation), "security: [ { daprCaller: [] }, { workloadPrincipal: [] } ]")
	}
	assertContains(t, document, "name: X-MS-CLIENT-PRINCIPAL")

	for _, operation := range []documentedOperation{
		{"GET", "/api/assets/collections", ""},
		{"POST", "/api/assets/sync-receipts", ""},
		{"GET", "/api/assets/collections/{collectionID}/changes", ""},
		{"GET", "/api/assets/collections/{collectionID}/items/{itemID}", ""},
		{"POST", "/api/assets/collections/{collectionID}/items/{itemID}/content-ticket", ""},
		{"GET", "/api/assets/collections/{collectionID}/items/{itemID}/content", ""},
	} {
		assertContains(t, operationBlockFor(t, document, operation), "security: [ { gatewayDaprCaller: [], gatewayTrustedIdentity: [] } ]")
	}
	receipt := operationBlockFor(t, document, documentedOperation{"POST", "/api/assets/sync-receipts", ""})
	for _, value := range []string{"SyncReceipt", "Receipt recorded", "no user, filename, path, meeting, or body data"} {
		assertContains(t, receipt, value)
	}
	for _, value := range []string{"additionalProperties: false", "required: [collectionItemId, contentVersion, state, appVersion]", "enum: [available-offline]", "maxLength: 64"} {
		assertContains(t, schemaBlockFor(t, document, "SyncReceipt"), value)
	}
	assertContains(t, operationBlockFor(t, document, documentedOperation{"GET", "/api/assets/content", ""}), "security: [ { gatewayDaprCaller: [] } ]")
	for _, value := range []string{"name: dapr-api-token", "Dapr-Caller-App-Id exactly `api-gateway`", "name: X-HHC-User-ID", "X-HHC-Token-Expires-At", "X-HHC-Role-IDs", "X-HHC-Token-ID", "X-HHC-Session-ID", "X-HHC-Auth-Provider"} {
		assertContains(t, document, value)
	}

	for _, test := range []struct {
		operation documentedOperation
		schema    string
	}{
		{documentedOperation{"GET", "/api/assets/collections", ""}, "ReaderCollectionPage"},
		{documentedOperation{"GET", "/api/assets/collections/{collectionID}/changes", ""}, "ReaderCollectionChanges"},
		{documentedOperation{"GET", "/api/assets/collections/{collectionID}/items/{itemID}", ""}, "ReaderCollectionItem"},
	} {
		assertContains(t, operationBlockFor(t, document, test.operation), test.schema)
	}
	for _, schema := range []string{"ReaderCollection", "ReaderCollectionItem"} {
		block := schemaBlockFor(t, document, schema)
		if strings.Contains(block, "retentionDays") || strings.Contains(block, "assetId") {
			t.Fatalf("%s exposes management metadata", schema)
		}
	}

	for _, operation := range []documentedOperation{
		{"POST", "/priv/assets/collections", ""},
		{"PATCH", "/priv/assets/collections/{collectionID}", ""},
		{"DELETE", "/priv/assets/collections/{collectionID}", ""},
		{"PATCH", "/priv/assets/collections/{collectionID}/retention", ""},
		{"POST", "/priv/assets/collections/{collectionID}/acl", ""},
		{"DELETE", "/priv/assets/collections/{collectionID}/acl/{aclID}", ""},
		{"POST", "/priv/assets/collections/{collectionID}/items", ""},
		{"POST", "/priv/assets/collections/{collectionID}/items/retention", ""},
		{"POST", "/priv/assets/collections/{collectionID}/items/delete", ""},
		{"PATCH", "/priv/assets/collections/{collectionID}/items/{itemID}", ""},
		{"DELETE", "/priv/assets/collections/{collectionID}/items/{itemID}", ""},
	} {
		assertContains(t, operationBlockFor(t, document, operation), "$ref: '#/components/parameters/IdempotencyKey'")
	}
	acl := operationBlockFor(t, document, documentedOperation{"POST", "/priv/assets/collections/{collectionID}/acl", ""})
	assertContains(t, acl, "CollectionACLRequest")
	assertContains(t, acl, "$ref: '#/components/parameters/ACLActorUserID'")
	assertContains(t, acl, "$ref: '#/components/parameters/RequestID'")
	revokeACL := operationBlockFor(t, document, documentedOperation{"DELETE", "/priv/assets/collections/{collectionID}/acl/{aclID}", ""})
	assertContains(t, revokeACL, "$ref: '#/components/parameters/ACLActorUserID'")
	assertContains(t, revokeACL, "$ref: '#/components/parameters/RequestID'")
	if strings.Contains(acl, "CreateGrantRequest") {
		t.Fatal("collection ACL accepts the asset-grant request body")
	}

	assertContains(t, schemaBlockFor(t, document, "ContentTicketBatch"), "ManagedContentTicket")
	assertContains(t, schemaBlockFor(t, document, "ManagedContentTicket"), "itemId")
	ticket := operationBlockFor(t, document, documentedOperation{"GET", "/api/assets/content", ""})
	assertContains(t, ticket, "$ref: '#/components/parameters/ContentTicket'")
	for _, operation := range []documentedOperation{
		{"GET", "/api/assets/public/{assetID}", ""},
		{"GET", "/api/assets/public/{assetID}/{variant}", ""},
		{"GET", "/api/assets/collections/{collectionID}/items/{itemID}/content", ""},
		{"GET", "/api/assets/content", ""},
		{"GET", "/priv/assets/{assetID}/download", ""},
	} {
		assertContains(t, operationBlockFor(t, document, operation), "'416': { $ref: '#/components/responses/Error' }")
	}
	assertContains(t, schemaBlockFor(t, document, "ErrorResponse"), "AST_INVALID_RANGE")
	for _, operation := range []documentedOperation{
		{"GET", "/api/assets/collections/{collectionID}/changes", ""},
		{"GET", "/api/assets/collections/{collectionID}/items/{itemID}", ""},
		{"POST", "/api/assets/collections/{collectionID}/items/{itemID}/content-ticket", ""},
		{"GET", "/api/assets/collections/{collectionID}/items/{itemID}/content", ""},
	} {
		assertContains(t, operationBlockFor(t, document, operation), "'404': { $ref: '#/components/responses/Error' }")
	}
}

func TestOpenAPILifecycleRequestInputs(t *testing.T) {
	raw, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	document := string(raw)

	upload := operationBlockFor(t, document, documentedOperation{"POST", "/priv/assets/upload-sessions", ""})
	assertContains(t, upload, "$ref: '#/components/parameters/IdempotencyKey'")
	assertContains(t, document, "IdempotencyKey: { name: Idempotency-Key, in: header, required: true")

	grant := operationBlockFor(t, document, documentedOperation{"POST", "/priv/assets/{assetID}/grants", ""})
	assertContains(t, grant, "$ref: '#/components/parameters/GrantIdempotencyKey'")
	assertContains(t, grant, "At least one of body idempotencyKey or Idempotency-Key header is required.")
	assertContains(t, document, "GrantIdempotencyKey: { name: Idempotency-Key, in: header, required: false")
	grantRequest := schemaBlockFor(t, document, "CreateGrantRequest")
	assertContains(t, grantRequest, "required: [subjectType, subjectId, permission]")
	assertContains(t, grantRequest, "idempotencyKey: { type: string, description: Required when Idempotency-Key header is absent. }")

	download := operationBlockFor(t, document, documentedOperation{"GET", "/priv/assets/{assetID}/download", ""})
	assertContains(t, download, "$ref: '#/components/parameters/AssetSubjectType'")
	assertContains(t, download, "$ref: '#/components/parameters/AssetSubjectID'")
	assertContains(t, document, "AssetSubjectType: { name: X-Asset-Subject-Type, in: header, required: true")
	assertContains(t, document, "AssetSubjectID: { name: X-Asset-Subject-Id, in: header, required: true")
}

func assertOperation(t *testing.T, document string, operation documentedOperation) {
	t.Helper()
	block := operationBlockFor(t, document, operation)
	if !strings.Contains(block, "x-hhc-callers: "+operation.callers) {
		t.Fatalf("%s %s callers=%q", operation.method, operation.path, operation.callers)
	}
}

func operationBlockFor(t *testing.T, document string, operation documentedOperation) string {
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
	return operationBody(rest)
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

func schemaBlockFor(t *testing.T, document, name string) string {
	t.Helper()
	start := strings.Index(document, "    "+name+":\n")
	if start < 0 {
		t.Fatalf("missing schema %s", name)
	}
	return operationBody(document[start:])
}

func assertContains(t *testing.T, value, want string) {
	t.Helper()
	if !strings.Contains(value, want) {
		t.Fatalf("missing %q", want)
	}
}
