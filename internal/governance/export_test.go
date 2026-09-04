package governance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const assetModuleName = "hhc/asset-api"

func assetModule(root string) (string, error) {
	command := exec.Command("go", "list", "-m")
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("resolve Go module: %w", err)
	}
	module := strings.TrimSpace(string(output))
	if module != assetModuleName {
		return "", fmt.Errorf("unexpected Go module %q", module)
	}
	return module, nil
}

func verifyEvidence(document map[string]any, events io.Reader, module string) error {
	type key struct{ pkg, test string }
	expected := map[key]bool{}
	for _, value := range document["datasets"].([]any) {
		dataset := value.(map[string]any)
		cleanup := dataset["cleanup"].(map[string]any)
		for _, value := range cleanup["evidence"].([]any) {
			evidence := value.(map[string]any)
			if evidence["kind"] != "go_test" {
				return fmt.Errorf("only executed Go evidence is supported")
			}
			pkg := path.Join(module, path.Dir(evidence["path"].(string)))
			expected[key{pkg, evidence["test_name"].(string)}] = true
			expected[key{pkg, ""}] = true
		}
	}
	passed, rejected := map[key]bool{}, map[key]bool{}
	decoder := json.NewDecoder(events)
	for {
		var event *struct{ Action, Package, Test string }
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			return fmt.Errorf("invalid Go test event stream: %w", err)
		}
		if event == nil {
			return fmt.Errorf("null Go test event")
		}
		item := key{event.Package, event.Test}
		if expected[item] {
			switch event.Action {
			case "pass":
				if passed[item] {
					rejected[item] = true
				}
				passed[item] = true
			case "skip", "fail":
				rejected[item] = true
			}
		}
	}
	for item := range expected {
		if !passed[item] || rejected[item] {
			return fmt.Errorf("missing, skipped or failed evidence: %s %s", item.pkg, item.test)
		}
	}
	return nil
}

func exportManifest(root string, raw []byte, events io.Reader, output string) error {
	document, err := validateManifest(root, raw, "asset-api")
	if err != nil {
		return err
	}
	module, err := assetModule(root)
	if err != nil {
		return err
	}
	if err := verifyEvidence(document, events, module); err != nil {
		return err
	}
	normalized, err := normalizeManifest(root, raw, "asset-api")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || (entry.Name() != "data-governance.yaml" && entry.Name() != "data-governance.json") {
			return fmt.Errorf("unexpected export entry %q", entry.Name())
		}
	}
	for name, payload := range map[string][]byte{"data-governance.yaml": raw, "data-governance.json": normalized} {
		if err := os.WriteFile(filepath.Join(output, name), payload, 0600); err != nil {
			return err
		}
	}
	return nil
}

func TestDataGovernanceExport(t *testing.T) {
	eventsPath, output := os.Getenv("GOVERNANCE_TEST_EVENTS"), os.Getenv("GOVERNANCE_OUTPUT_DIR")
	if eventsPath == "" && output == "" {
		t.Skip("explicit evidence/export paths are not configured")
	}
	require.NotEmpty(t, eventsPath, "GOVERNANCE_TEST_EVENTS is required")
	require.NotEmpty(t, output, "GOVERNANCE_OUTPUT_DIR is required")
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(root, "docs/data-governance.yaml"))
	require.NoError(t, err)
	events, err := os.Open(eventsPath)
	require.NoError(t, err)
	defer events.Close()
	require.NoError(t, exportManifest(root, raw, events, output))
}

func TestDataGovernanceEvidence(t *testing.T) {
	raw, err := os.ReadFile("testdata/enforced-asset.json")
	require.NoError(t, err)
	document, err := validateManifest("../..", raw, "asset-api")
	require.NoError(t, err)
	module, err := assetModule("../..")
	require.NoError(t, err)
	const postgresUpload = `{"Action":"pass","Package":"hhc/asset-api/internal/postgres","Test":"TestAccountUploadExpiryEligibilityIsStrictAndLeaseBounded"}` + "\n"
	const assetsDelete = `{"Action":"pass","Package":"hhc/asset-api/internal/assets","Test":"TestSoftDeleteImmediatelyBlocksPublicDownload"}` + "\n"
	const lifecycleEveryObject = `{"Action":"pass","Package":"hhc/asset-api/internal/lifecycle","Test":"TestWorkerPurgesEveryCandidateObjectAndCompletes"}` + "\n"
	const lifecycleRetry = `{"Action":"pass","Package":"hhc/asset-api/internal/lifecycle","Test":"TestWorkerRetriesBlobFailure"}` + "\n"
	const azureMissing = `{"Action":"pass","Package":"hhc/asset-api/internal/storage/azure","Test":"TestDeleteMissingBlobIsRepeatSafe"}` + "\n"
	const postgresExpiry = `{"Action":"pass","Package":"hhc/asset-api/internal/postgres","Test":"TestDeleteExpiredPurgeIsBoundedAndPreservesRecentOrActiveAssets"}` + "\n"
	const postgresDerivativeKeys = `{"Action":"pass","Package":"hhc/asset-api/internal/postgres","Test":"TestPurgeIncludesAttemptSpecificDerivativeKeys"}` + "\n"
	const assetsPackage = `{"Action":"pass","Package":"hhc/asset-api/internal/assets"}` + "\n"
	const lifecyclePackage = `{"Action":"pass","Package":"hhc/asset-api/internal/lifecycle"}` + "\n"
	const azurePackage = `{"Action":"pass","Package":"hhc/asset-api/internal/storage/azure"}` + "\n"
	const postgresPackage = `{"Action":"pass","Package":"hhc/asset-api/internal/postgres"}` + "\n"
	testsPassed := postgresUpload + assetsDelete + lifecycleEveryObject + lifecycleRetry + azureMissing + postgresExpiry + postgresDerivativeKeys
	packagesPassed := assetsPackage + lifecyclePackage + azurePackage + postgresPackage
	passed := testsPassed + packagesPassed
	for _, test := range []struct {
		name, events string
		valid        bool
	}{
		{"passed tests and packages", passed, true},
		{"integration event absent", assetsDelete + lifecycleEveryObject + lifecycleRetry + azureMissing + postgresExpiry + postgresDerivativeKeys + packagesPassed, false},
		{"missing", packagesPassed, false},
		{"empty", "", false},
		{"integration skipped without database", strings.ReplaceAll(passed, `"pass"`, `"skip"`), false},
		{"failed", strings.ReplaceAll(passed, `"pass"`, `"fail"`), false},
		{"missing package result", testsPassed, false},
		{"failed package", passed + strings.ReplaceAll(postgresPackage, `"pass"`, `"fail"`), false},
		{"skipped package", passed + strings.ReplaceAll(postgresPackage, `"pass"`, `"skip"`), false},
		{"other package", strings.ReplaceAll(passed, "/internal/postgres", "/internal/store"), false},
		{"other module", strings.ReplaceAll(passed, module, "example.invalid/asset-api"), false},
		{"name prefix", strings.ReplaceAll(passed, "BoundedAndPreservesRecentOrActiveAssets", "BoundedAndPreservesRecentOrActiveAssetsOther"), false},
		{"subtest only", strings.ReplaceAll(passed, "BoundedAndPreservesRecentOrActiveAssets", "BoundedAndPreservesRecentOrActiveAssets/child"), false},
		{"failed then passed", strings.ReplaceAll(passed, `"pass"`, `"fail"`) + passed, false},
		{"skipped then passed", strings.ReplaceAll(passed, `"pass"`, `"skip"`) + passed, false},
		{"package failed then passed", passed + strings.ReplaceAll(postgresPackage, `"pass"`, `"fail"`) + postgresPackage, false},
		{"duplicate test pass", passed + postgresUpload, false},
		{"duplicate package pass", passed + postgresPackage, false},
		{"truncated", passed + `{"Action":`, false},
		{"non JSON", passed + "broken", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := verifyEvidence(document, strings.NewReader(test.events), module)
			require.Equal(t, test.valid, err == nil, "%v", err)
		})
	}
	t.Run("shell metadata is not execution", func(t *testing.T) {
		replaceLeaf(t, document, []string{"datasets", "0", "cleanup", "evidence", "0", "kind"}, "shell_test")
		require.Error(t, verifyEvidence(document, strings.NewReader(passed), module))
	})
}

func TestDataGovernanceExportPayload(t *testing.T) {
	raw, err := os.ReadFile("testdata/enforced-asset.json")
	require.NoError(t, err)
	const events = `{"Action":"pass","Package":"hhc/asset-api/internal/postgres","Test":"TestAccountUploadExpiryEligibilityIsStrictAndLeaseBounded"}
{"Action":"pass","Package":"hhc/asset-api/internal/assets","Test":"TestSoftDeleteImmediatelyBlocksPublicDownload"}
{"Action":"pass","Package":"hhc/asset-api/internal/lifecycle","Test":"TestWorkerPurgesEveryCandidateObjectAndCompletes"}
{"Action":"pass","Package":"hhc/asset-api/internal/lifecycle","Test":"TestWorkerRetriesBlobFailure"}
{"Action":"pass","Package":"hhc/asset-api/internal/storage/azure","Test":"TestDeleteMissingBlobIsRepeatSafe"}
{"Action":"pass","Package":"hhc/asset-api/internal/postgres","Test":"TestDeleteExpiredPurgeIsBoundedAndPreservesRecentOrActiveAssets"}
{"Action":"pass","Package":"hhc/asset-api/internal/postgres","Test":"TestPurgeIncludesAttemptSpecificDerivativeKeys"}
{"Action":"pass","Package":"hhc/asset-api/internal/assets"}
{"Action":"pass","Package":"hhc/asset-api/internal/lifecycle"}
{"Action":"pass","Package":"hhc/asset-api/internal/storage/azure"}
{"Action":"pass","Package":"hhc/asset-api/internal/postgres"}`
	out := filepath.Join(t.TempDir(), "export")
	require.Error(t, exportManifest("../..", raw, strings.NewReader(""), out))
	_, err = os.Stat(out)
	require.True(t, os.IsNotExist(err), "rejected evidence must not produce artifacts")
	require.Error(t, exportManifest("../..", raw, strings.NewReader(strings.ReplaceAll(events, `"pass"`, `"skip"`)), out))
	_, err = os.Stat(out)
	require.True(t, os.IsNotExist(err), "skipped evidence must not produce artifacts")
	unsafeOut := filepath.Join(t.TempDir(), "unsafe-export")
	unsafe := bytes.Replace(raw, []byte(`"path": "internal/postgres/store_integration_test.go"`), []byte(`"path": "../go.mod"`), 1)
	require.Error(t, exportManifest("../..", unsafe, strings.NewReader(events), unsafeOut))
	_, err = os.Stat(unsafeOut)
	require.True(t, os.IsNotExist(err), "untrusted evidence must not produce artifacts")
	require.NoError(t, exportManifest("../..", raw, strings.NewReader(events), out))
	yamlBytes, err := os.ReadFile(filepath.Join(out, "data-governance.yaml"))
	require.NoError(t, err)
	require.Equal(t, raw, yamlBytes)
	jsonBytes, err := os.ReadFile(filepath.Join(out, "data-governance.json"))
	require.NoError(t, err)
	var sourceYAML any
	require.NoError(t, yaml.Unmarshal(raw, &sourceYAML))
	sourceJSON, err := json.Marshal(sourceYAML)
	require.NoError(t, err)
	var sourceDocument, exportedDocument any
	require.NoError(t, json.Unmarshal(sourceJSON, &sourceDocument))
	require.NoError(t, json.Unmarshal(jsonBytes, &exportedDocument))
	require.Equal(t, sourceDocument, exportedDocument)
	entries, err := os.ReadDir(out)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.NoError(t, exportManifest("../..", raw, strings.NewReader(events), out), "rerun")
	require.NoError(t, os.WriteFile(filepath.Join(out, "test-events.jsonl"), []byte("must not publish"), 0600))
	require.Error(t, exportManifest("../..", raw, strings.NewReader(events), out), "unexpected payload")
}
