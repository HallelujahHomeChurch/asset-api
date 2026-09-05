package governance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const manifestService = "asset-api"

func validateManifest(root string, raw []byte, service string) (map[string]any, error) {
	if len(raw) > 1<<20 {
		return nil, fmt.Errorf("manifest exceeds 1 MiB")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var node yaml.Node
	if err := decoder.Decode(&node); err != nil {
		return nil, err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("expected one YAML document: %v", err)
	}
	if len(node.Content) != 1 {
		return nil, fmt.Errorf("expected a manifest mapping")
	}
	if err := validateYAMLNode(node.Content[0], 1); err != nil {
		return nil, err
	}
	var document map[string]any
	if err := node.Decode(&document); err != nil {
		return nil, err
	}
	if err := validateObject(root, document, service); err != nil {
		return nil, err
	}
	// The JSON representation is shared by YAML and JSON consumers.
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	jsonDecoder := json.NewDecoder(bytes.NewReader(raw))
	jsonDecoder.UseNumber()
	if err := jsonDecoder.Decode(&document); err != nil {
		return nil, err
	}
	return document, nil
}

func normalizeManifest(root string, raw []byte, service string) ([]byte, error) {
	document, err := validateManifest(root, raw, service)
	if err != nil {
		return nil, err
	}
	exported, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(exported, '\n'), nil
}

func validateYAMLNode(node *yaml.Node, depth int) error {
	if depth > 16 || node.Anchor != "" || node.Kind == yaml.AliasNode {
		return fmt.Errorf("YAML depth, anchor or alias is forbidden")
	}
	switch node.Kind {
	case yaml.MappingNode:
		if node.Tag != "!!map" {
			return fmt.Errorf("custom mapping tag")
		}
		seen := map[string]bool{}
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "<<" || key.Anchor != "" || seen[key.Value] {
				return fmt.Errorf("invalid or duplicate YAML key %q", key.Value)
			}
			seen[key.Value] = true
			if err := validateYAMLNode(node.Content[index+1], depth+1); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		if node.Tag != "!!seq" {
			return fmt.Errorf("custom sequence tag")
		}
		for _, child := range node.Content {
			if err := validateYAMLNode(child, depth+1); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if !oneOf(node.Tag, "!!str !!null !!int !!bool !!float") {
			return fmt.Errorf("unsupported YAML tag %q", node.Tag)
		}
	default:
		return fmt.Errorf("unsupported YAML node")
	}
	return nil
}

func object(value any, keys string) (map[string]any, error) {
	mapping, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected object")
	}
	want := strings.Fields(keys)
	if len(mapping) != len(want) {
		return nil, fmt.Errorf("expected exact keys %s", keys)
	}
	for _, key := range want {
		if _, ok := mapping[key]; !ok {
			return nil, fmt.Errorf("missing key %s", key)
		}
	}
	return mapping, nil
}

func list(value any, minimum int) ([]any, error) {
	values, ok := value.([]any)
	if !ok || len(values) < minimum {
		return nil, fmt.Errorf("expected array with at least %d entries", minimum)
	}
	return values, nil
}

func nonempty(value any) bool { text, ok := value.(string); return ok && strings.TrimSpace(text) != "" }
func oneOf(value any, choices string) bool {
	text, ok := value.(string)
	if !ok {
		return false
	}
	for _, choice := range strings.Fields(choices) {
		if text == choice {
			return true
		}
	}
	return false
}

func classes(value any) error {
	values, err := list(value, 1)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, value := range values {
		if !oneOf(value, "identity contact security consent activity content financial") {
			return fmt.Errorf("invalid data class")
		}
		name := value.(string)
		if seen[name] {
			return fmt.Errorf("duplicate data class %s", name)
		}
		seen[name] = true
	}
	return nil
}

func validateObject(root string, document map[string]any, service string) error {
	if _, err := object(document, "schema_version service datasets"); err != nil {
		return err
	}
	switch version := document["schema_version"].(type) {
	case int:
		if version != 1 {
			return fmt.Errorf("unsupported schema version")
		}
	case json.Number:
		if version != "1" {
			return fmt.Errorf("unsupported schema version")
		}
	default:
		return fmt.Errorf("schema version must be integer 1")
	}
	prefix := map[string]string{"account-api": "account.", "engagement-api": "engagement.", "notification-api": "notification.", "asset-api": "asset.", "hhc-web-api": "hhc-web."}[service]
	if prefix == "" || document["service"] != service {
		return fmt.Errorf("unknown or mismatched service")
	}
	datasets, err := list(document["datasets"], 1)
	if err != nil {
		return err
	}
	if len(datasets) > 200 {
		return fmt.Errorf("too many datasets")
	}
	seen := map[string]bool{}
	totalFields := 0
	for _, value := range datasets {
		dataset, err := object(value, "id owner purpose necessity storage subject_keys data_classes fields attribution legal_basis retention cleanup")
		if err != nil {
			return err
		}
		if !nonempty(dataset["id"]) || !nonempty(dataset["purpose"]) || !oneOf(dataset["necessity"], "required optional") || dataset["owner"] != service {
			return fmt.Errorf("invalid dataset identity, purpose, necessity or owner")
		}
		id := dataset["id"].(string)
		if !strings.HasPrefix(id, prefix) || strings.TrimSpace(strings.TrimPrefix(id, prefix)) == "" || seen[id] {
			return fmt.Errorf("invalid or duplicate dataset ID %q", id)
		}
		seen[id] = true
		if err := validateDataset(root, dataset); err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
		totalFields += len(dataset["fields"].([]any))
		if totalFields > 2000 {
			return fmt.Errorf("too many fields")
		}
	}
	return nil
}

func validateDataset(root string, dataset map[string]any) error {
	storage, err := object(dataset["storage"], "kind resource sources")
	if err != nil {
		return err
	}
	if !oneOf(storage["kind"], "postgres redis blob") || !nonempty(storage["resource"]) {
		return fmt.Errorf("invalid storage")
	}
	if err := sourceRefs(root, storage["sources"], 1); err != nil {
		return err
	}
	if err := classes(dataset["data_classes"]); err != nil {
		return err
	}
	fields, err := list(dataset["fields"], 1)
	if err != nil {
		return err
	}
	seenFields := map[string]bool{}
	for _, value := range fields {
		field, err := object(value, "name purpose necessity data_classes")
		if err != nil {
			return err
		}
		if !nonempty(field["name"]) || !nonempty(field["purpose"]) || !oneOf(field["necessity"], "required optional") {
			return fmt.Errorf("invalid personal field metadata")
		}
		name := field["name"].(string)
		if seenFields[name] {
			return fmt.Errorf("duplicate field %s", name)
		}
		seenFields[name] = true
		if err := classes(field["data_classes"]); err != nil {
			return err
		}
	}
	subjects, err := list(dataset["subject_keys"], 0)
	if err != nil {
		return err
	}
	kinds := map[string]bool{}
	for _, value := range subjects {
		subject, err := object(value, "kind field evidence")
		if err != nil {
			return err
		}
		if !oneOf(subject["kind"], "user_id verified_canonical_email") || !nonempty(subject["field"]) {
			return fmt.Errorf("invalid subject key")
		}
		if err := sourceRefs(root, subject["evidence"], 1); err != nil {
			return err
		}
		kinds[subject["kind"].(string)] = true
	}
	attribution, err := object(dataset["attribution"], "mode explanation")
	if err != nil {
		return err
	}
	if !nonempty(attribution["explanation"]) {
		return fmt.Errorf("missing attribution explanation")
	}
	switch attribution["mode"] {
	case "direct":
		if !kinds["user_id"] {
			return fmt.Errorf("direct attribution requires user_id")
		}
	case "verified_lookup":
		if !kinds["verified_canonical_email"] {
			return fmt.Errorf("lookup requires verified canonical email")
		}
	case "manual", "none":
		if len(subjects) != 0 {
			return fmt.Errorf("manual/none cannot claim deterministic subject keys")
		}
	default:
		return fmt.Errorf("invalid attribution mode")
	}
	legal, err := object(dataset["legal_basis"], "status reference")
	if err != nil {
		return err
	}
	if legal["status"] != "pending_legal" || legal["reference"] != nil {
		return fmt.Errorf("legal basis must remain pending")
	}
	retention, err := object(dataset["retention"], "status rule action")
	if err != nil {
		return err
	}
	cleanup, err := object(dataset["cleanup"], "mode implementation evidence")
	if err != nil {
		return err
	}
	if !oneOf(retention["status"], "enforced pending_legal") || !oneOf(retention["action"], "none expire delete purge deidentify") || !oneOf(cleanup["mode"], "none native_ttl existing_worker existing_command") {
		return fmt.Errorf("invalid retention or cleanup enum")
	}
	implementation, err := list(cleanup["implementation"], 0)
	if err != nil {
		return err
	}
	evidence, err := list(cleanup["evidence"], 0)
	if err != nil {
		return err
	}
	if retention["status"] == "pending_legal" {
		if retention["rule"] != nil || retention["action"] != "none" || cleanup["mode"] != "none" || len(implementation) != 0 || len(evidence) != 0 {
			return fmt.Errorf("pending retention cannot claim cleanup")
		}
		return nil
	}
	if retention["rule"] == nil || retention["action"] == "none" || cleanup["mode"] == "none" || len(implementation) == 0 || len(evidence) == 0 {
		return fmt.Errorf("enforced retention requires rule, action, implementation and evidence")
	}
	if cleanup["mode"] == "native_ttl" && (storage["kind"] != "redis" || retention["action"] != "expire") {
		return fmt.Errorf("native TTL requires Redis expire")
	}
	rule, err := object(retention["rule"], "trigger field source duration_source predicate description")
	if err != nil {
		return err
	}
	if !nonempty(rule["field"]) || !nonempty(rule["predicate"]) || !nonempty(rule["description"]) {
		return fmt.Errorf("invalid retention rule metadata")
	}
	if err := sourceRef(root, rule["source"]); err != nil {
		return err
	}
	switch rule["trigger"] {
	case "age_since":
		if err := sourceRef(root, rule["duration_source"]); err != nil {
			return err
		}
	case "expires_at":
		if rule["duration_source"] != nil {
			return fmt.Errorf("expires_at cannot have duration_source")
		}
	default:
		return fmt.Errorf("invalid retention trigger")
	}
	if err := sourceRefs(root, cleanup["implementation"], 1); err != nil {
		return err
	}
	for _, value := range evidence {
		if err := evidenceRef(root, value); err != nil {
			return err
		}
	}
	return nil
}

func sourceRefs(root string, value any, minimum int) error {
	refs, err := list(value, minimum)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if err := sourceRef(root, ref); err != nil {
			return err
		}
	}
	return nil
}

var sourceSymbol = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.:-]*$`)

func sourceRef(root string, value any) error {
	ref, err := object(value, "path symbol")
	if err != nil {
		return err
	}
	if !nonempty(ref["path"]) || !nonempty(ref["symbol"]) || !sourceSymbol.MatchString(ref["symbol"].(string)) {
		return fmt.Errorf("source reference must name a symbol, not a command")
	}
	path, err := checkedPath(root, ref["path"].(string))
	if err != nil {
		return err
	}
	if filepath.Ext(path) == ".go" {
		return declaredGoSymbol(path, ref["symbol"].(string), false)
	}
	// SQL/config meaning is asserted by the owner, not inferred from file existence.
	return nil
}

func evidenceRef(root string, value any) error {
	evidence, err := object(value, "kind path test_name")
	if err != nil {
		return err
	}
	if !nonempty(evidence["path"]) {
		return fmt.Errorf("missing evidence path")
	}
	path, err := checkedPath(root, evidence["path"].(string))
	if err != nil {
		return err
	}
	switch evidence["kind"] {
	case "go_test":
		if !strings.HasSuffix(path, "_test.go") || !nonempty(evidence["test_name"]) {
			return fmt.Errorf("Go evidence needs a declared test")
		}
		return declaredGoSymbol(path, evidence["test_name"].(string), true)
	case "shell_test":
		if evidence["test_name"] != nil {
			return fmt.Errorf("shell evidence needs null test_name")
		}
		return nil
	default:
		return fmt.Errorf("invalid evidence kind")
	}
}

func checkedPath(root, name string) (string, error) {
	if name == "." || strings.Contains(name, "\\") || !fs.ValidPath(name) {
		return "", fmt.Errorf("unsafe reference %q", name)
	}
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	target, err := filepath.EvalSymlinks(filepath.Join(base, name))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("reference escapes repository")
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("reference is not a file")
	}
	return target, nil
}

func declaredGoSymbol(path, symbol string, testOnly bool) error {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return err
	}
	testingAlias := ""
	for _, spec := range file.Imports {
		if spec.Path.Value == `"testing"` {
			testingAlias = "testing"
			if spec.Name != nil {
				testingAlias = spec.Name.Name
			}
		}
	}
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			name := decl.Name.Name
			if decl.Recv != nil {
				receiver := decl.Recv.List[0].Type
				if pointer, ok := receiver.(*ast.StarExpr); ok {
					receiver = pointer.X
				}
				if identifier, ok := receiver.(*ast.Ident); ok && symbol == identifier.Name+"."+name && !testOnly {
					return nil
				}
			}
			if symbol != name {
				continue
			}
			if !testOnly {
				return nil
			}
			if decl.Recv != nil || !strings.HasPrefix(name, "Test") || (len(name) > 4 && unicode.IsLower([]rune(name)[4])) || decl.Type.TypeParams != nil || (decl.Type.Results != nil && len(decl.Type.Results.List) != 0) || len(decl.Type.Params.List) != 1 {
				continue
			}
			parameter := decl.Type.Params.List[0]
			if len(parameter.Names) > 1 {
				continue
			}
			pointer, ok := parameter.Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			selector, ok := pointer.X.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "T" {
				continue
			}
			alias, ok := selector.X.(*ast.Ident)
			if ok && alias.Name == testingAlias {
				return nil
			}
		case *ast.GenDecl:
			if testOnly {
				continue
			}
			for _, spec := range decl.Specs {
				switch spec := spec.(type) {
				case *ast.ValueSpec:
					for _, name := range spec.Names {
						if symbol == name.Name {
							return nil
						}
					}
				case *ast.TypeSpec:
					if symbol == spec.Name.Name {
						return nil
					}
					if structure, ok := spec.Type.(*ast.StructType); ok {
						for _, field := range structure.Fields.List {
							for _, name := range field.Names {
								if symbol == spec.Name.Name+"."+name.Name {
									return nil
								}
							}
						}
					}
				}
			}
		}
	}
	return fmt.Errorf("Go declaration %q not found in %s", symbol, path)
}

func fixtureManifest(t *testing.T) (string, map[string]any) {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module fixture\n"), 0600))
	raw, err := os.ReadFile("testdata/conformance.json")
	require.NoError(t, err)
	var fixture struct{ Base map[string]any }
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&fixture))
	fixture.Base["service"] = manifestService
	dataset := fixture.Base["datasets"].([]any)[0].(map[string]any)
	dataset["id"] = "asset.fixture"
	dataset["owner"] = manifestService
	return root, fixture.Base
}

func replaceLeaf(t *testing.T, document any, path []string, value any) {
	t.Helper()
	for _, key := range path[:len(path)-1] {
		switch node := document.(type) {
		case map[string]any:
			document = node[key]
		case []any:
			index, err := strconv.Atoi(key)
			require.NoError(t, err)
			document = node[index]
		default:
			t.Fatalf("invalid fixture path %v", path)
		}
	}
	key := path[len(path)-1]
	switch node := document.(type) {
	case map[string]any:
		node[key] = value
	case []any:
		index, err := strconv.Atoi(key)
		require.NoError(t, err)
		node[index] = value
	default:
		t.Fatalf("invalid fixture leaf %v", path)
	}
}

func TestDataGovernanceConformance(t *testing.T) {
	raw, err := os.ReadFile("testdata/conformance.json")
	require.NoError(t, err)
	var fixture struct {
		Version int
		Cases   []struct {
			Name  string
			Path  []string
			Value any
			Valid bool
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&fixture))
	require.Equal(t, 1, fixture.Version)
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			root, document := fixtureManifest(t)
			replaceLeaf(t, document, test.Path, test.Value)
			err := validateObject(root, document, manifestService)
			require.Equal(t, test.Valid, err == nil, "validation: %v", err)
			raw, err := json.Marshal(document)
			require.NoError(t, err)
			_, err = validateManifest(root, raw, manifestService)
			require.Equal(t, test.Valid, err == nil, "decode: %v", err)
		})
	}
}

func TestDataGovernanceRejectsMultipleDocuments(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, document := fixtureManifest(t)
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateManifest(root, raw, manifestService); err != nil {
		t.Fatalf("single-document control must be valid: %v", err)
	}
	raw = append(append([]byte(nil), raw...), []byte("\n---\n{}\n")...)
	if _, err := validateManifest(root, raw, manifestService); err == nil {
		t.Fatal("accepted multiple YAML documents")
	}
}

func TestDataGovernanceStrictYAML(t *testing.T) {
	root, document := fixtureManifest(t)
	raw, err := yaml.Marshal(document)
	require.NoError(t, err)
	// Use an integer for YAML, since the fixture's JSON number is a string alias.
	raw = bytes.Replace(raw, []byte("schema_version: \"1\""), []byte("schema_version: 1"), 1)
	_, err = validateManifest(root, raw, manifestService)
	require.NoError(t, err)
	for name, input := range map[string][]byte{
		"duplicate key":  append(append([]byte(nil), raw...), []byte("service: asset-api\n")...),
		"anchor":         bytes.Replace(raw, []byte("service: asset-api"), []byte("service: &owner asset-api"), 1),
		"alias":          append(append([]byte(nil), raw...), []byte("other: *owner\n")...),
		"merge":          append(append([]byte(nil), raw...), []byte("<<: {service: asset-api}\n")...),
		"custom tag":     bytes.Replace(raw, []byte("service: asset-api"), []byte("service: !service asset-api"), 1),
		"non-string key": append(append([]byte(nil), raw...), []byte("1: value\n")...),
		"boolean":        bytes.Replace(raw, []byte("service: asset-api"), []byte("service: true"), 1),
		"float version":  bytes.Replace(raw, []byte("schema_version: 1"), []byte("schema_version: 1.0"), 1),
		"timestamp":      bytes.Replace(raw, []byte("service: asset-api"), []byte("service: 2026-01-01"), 1),
		"null document":  []byte("null\n"),
		"depth":          []byte(strings.Repeat("[", 17) + "null" + strings.Repeat("]", 17)),
		"oversize":       bytes.Repeat([]byte(" "), (1<<20)+1),
	} {
		t.Run(name, func(t *testing.T) { _, err := validateManifest(root, input, manifestService); require.Error(t, err) })
	}
}

func TestDataGovernanceNormalization(t *testing.T) {
	root, document := fixtureManifest(t)
	raw, err := json.Marshal(document)
	require.NoError(t, err)
	exported, err := normalizeManifest(root, raw, manifestService)
	require.NoError(t, err)
	again, err := normalizeManifest(root, raw, manifestService)
	require.NoError(t, err)
	require.Equal(t, exported, again)
	require.True(t, bytes.HasSuffix(exported, []byte("\n")))
	var roundTrip map[string]any
	decoder := json.NewDecoder(bytes.NewReader(exported))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&roundTrip))
	validated, err := validateManifest(root, raw, manifestService)
	require.NoError(t, err)
	require.True(t, reflect.DeepEqual(document, roundTrip))
	require.True(t, reflect.DeepEqual(validated, roundTrip))
	replaceLeaf(t, document, []string{"datasets", "0", "fields", "0", "purpose"}, "Different purpose.")
	raw, err = json.Marshal(document)
	require.NoError(t, err)
	changed, err := normalizeManifest(root, raw, manifestService)
	require.NoError(t, err)
	require.NotEqual(t, exported, changed)
}

func enforcedManifest(t *testing.T) (string, map[string]any) {
	t.Helper()
	root, document := fixtureManifest(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "source.go"), []byte("package fixture\ntype Worker struct { TTL int }\nfunc (w *Worker) Cleanup() {}\nvar Window = 1\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "cleanup_test.go"), []byte("package fixture\nimport \"testing\"\nfunc TestCleanup(t *testing.T) {}\n// func TestComment(t *testing.T) {}\nvar TestVariable = 1\nfunc TestWrongSignature() {}\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "cleanup.sh"), []byte("#!/bin/sh\nexit 0\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "check"), []byte("#!/bin/sh\nexit 0\n"), 0600))
	ref := map[string]any{"path": "source.go", "symbol": "Worker.Cleanup"}
	replaceLeaf(t, document, []string{"datasets", "0", "retention"}, map[string]any{"status": "enforced", "rule": map[string]any{"trigger": "expires_at", "field": "expires_at", "source": ref, "duration_source": nil, "predicate": "expires_at <= now", "description": "On request, removes expired fixture."}, "action": "delete"})
	replaceLeaf(t, document, []string{"datasets", "0", "cleanup"}, map[string]any{"mode": "existing_command", "implementation": []any{ref}, "evidence": []any{map[string]any{"kind": "go_test", "path": "cleanup_test.go", "test_name": "TestCleanup"}}})
	return root, document
}

func TestDataGovernanceEnforcementAndReferences(t *testing.T) {
	for _, test := range []struct {
		name, path string
		value      any
		valid      bool
	}{
		{"valid enforced", "retention/action", "delete", true},
		{"valid worker", "cleanup/mode", "existing_worker", true},
		{"valid shell", "cleanup/evidence", []any{map[string]any{"kind": "shell_test", "path": "cleanup.sh", "test_name": nil}}, true},
		{"extensionless shell", "cleanup/evidence", []any{map[string]any{"kind": "shell_test", "path": "check", "test_name": nil}}, true},
		{"unknown rule", "retention/rule/other", true, false},
		{"missing implementation", "cleanup/implementation", []any{}, false},
		{"missing evidence", "cleanup/evidence", []any{}, false},
		{"null rule", "retention/rule", nil, false},
		{"none action", "retention/action", "none", false},
		{"none mode", "cleanup/mode", "none", false},
		{"age duration required", "retention/rule/trigger", "age_since", false},
		{"expires duration forbidden", "retention/rule/duration_source", map[string]any{"path": "go.mod", "symbol": "module"}, false},
		{"ttl only redis", "cleanup/mode", "native_ttl", false},
		{"comment not test", "cleanup/evidence/0/test_name", "TestComment", false},
		{"variable not test", "cleanup/evidence/0/test_name", "TestVariable", false},
		{"signature not test", "cleanup/evidence/0/test_name", "TestWrongSignature", false},
		{"missing source symbol", "cleanup/implementation/0/symbol", "Worker.Unknown", false},
		{"source field", "cleanup/implementation/0/symbol", "Worker.TTL", true},
		{"source variable", "cleanup/implementation/0/symbol", "Window", true},
		{"missing source", "storage/sources/0/path", "missing", false},
		{"absolute source", "storage/sources/0/path", "/tmp/go.mod", false},
		{"backslash source", "storage/sources/0/path", "dir\\go.mod", false},
		{"root source", "storage/sources/0/path", ".", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, document := enforcedManifest(t)
			require.NoError(t, validateObject(root, document, manifestService))
			replaceLeaf(t, document, append([]string{"datasets", "0"}, strings.Split(test.path, "/")...), test.value)
			err := validateObject(root, document, manifestService)
			require.Equal(t, test.valid, err == nil, "%v", err)
		})
	}
	root, document := enforcedManifest(t)
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(root, "escape")))
	// Resolve an existing outside file, not only a dangling link.
	outside, err := os.Readlink(filepath.Join(root, "escape"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0600))
	replaceLeaf(t, document, []string{"datasets", "0", "storage", "sources", "0", "path"}, "escape")
	require.Error(t, validateObject(root, document, manifestService))
	require.NoError(t, os.Mkdir(filepath.Join(root, "directory"), 0700))
	replaceLeaf(t, document, []string{"datasets", "0", "storage", "sources", "0", "path"}, "directory")
	require.Error(t, validateObject(root, document, manifestService))
}

func TestDataGovernanceLimits(t *testing.T) {
	for _, count := range []int{200, 201} {
		t.Run(fmt.Sprint("datasets-", count), func(t *testing.T) {
			root, document := fixtureManifest(t)
			var datasets []any
			for index := 0; index < count; index++ {
				_, next := fixtureManifest(t)
				item := next["datasets"].([]any)[0].(map[string]any)
				item["id"] = fmt.Sprintf("asset.fixture-%d", index)
				datasets = append(datasets, item)
			}
			document["datasets"] = datasets
			require.Equal(t, count <= 200, validateObject(root, document, manifestService) == nil)
		})
	}
	for _, count := range []int{2000, 2001} {
		root, document := fixtureManifest(t)
		fields := make([]any, count)
		for index := range fields {
			fields[index] = map[string]any{"name": fmt.Sprint("field", index), "purpose": "Personal field.", "necessity": "required", "data_classes": []any{"identity"}}
		}
		replaceLeaf(t, document, []string{"datasets", "0", "fields"}, fields)
		require.Equal(t, count <= 2000, validateObject(root, document, manifestService) == nil)
	}
}

func TestDataGovernanceExactObjectKeys(t *testing.T) {
	// Every object in a valid enforced manifest must reject unknown and absent keys.
	root, document := enforcedManifest(t)
	var visit func(any, []string)
	visit = func(value any, path []string) {
		switch node := value.(type) {
		case map[string]any:
			node["unexpected"] = true
			require.Error(t, validateObject(root, document, manifestService), "unknown at %v", path)
			delete(node, "unexpected")
			for key, child := range node {
				delete(node, key)
				require.Error(t, validateObject(root, document, manifestService), "missing %v/%s", path, key)
				node[key] = child
				visit(child, append(append([]string(nil), path...), key))
			}
		case []any:
			for index, child := range node {
				visit(child, append(append([]string(nil), path...), strconv.Itoa(index)))
			}
		}
	}
	require.NoError(t, validateObject(root, document, manifestService))
	visit(document, nil)
	require.NoError(t, validateObject(root, document, manifestService))
}

func TestDataGovernanceValidVariants(t *testing.T) {
	for _, service := range []string{"account-api", "engagement-api", "notification-api", "asset-api", "hhc-web-api"} {
		root, document := fixtureManifest(t)
		document["service"] = service
		replaceLeaf(t, document, []string{"datasets", "0", "owner"}, service)
		replaceLeaf(t, document, []string{"datasets", "0", "id"}, strings.TrimSuffix(service, "-api")+".fixture")
		require.NoError(t, validateObject(root, document, service))
	}
	for _, mode := range []string{"none", "manual", "verified_lookup"} {
		root, document := fixtureManifest(t)
		replaceLeaf(t, document, []string{"datasets", "0", "attribution", "mode"}, mode)
		if mode == "verified_lookup" {
			replaceLeaf(t, document, []string{"datasets", "0", "subject_keys", "0", "kind"}, "verified_canonical_email")
		} else {
			replaceLeaf(t, document, []string{"datasets", "0", "subject_keys"}, []any{})
		}
		require.NoError(t, validateObject(root, document, manifestService))
	}
	root, document := enforcedManifest(t)
	replaceLeaf(t, document, []string{"datasets", "0", "retention", "rule", "trigger"}, "age_since")
	replaceLeaf(t, document, []string{"datasets", "0", "retention", "rule", "duration_source"}, map[string]any{"path": "source.go", "symbol": "Worker.TTL"})
	require.NoError(t, validateObject(root, document, manifestService))
	replaceLeaf(t, document, []string{"datasets", "0", "cleanup", "mode"}, "native_ttl")
	replaceLeaf(t, document, []string{"datasets", "0", "storage", "kind"}, "redis")
	require.Error(t, validateObject(root, document, manifestService), "native TTL action must be expire")
	replaceLeaf(t, document, []string{"datasets", "0", "retention", "action"}, "expire")
	require.NoError(t, validateObject(root, document, manifestService))
	require.NoError(t, os.Symlink(filepath.Join(root, "go.mod"), filepath.Join(root, "inside")))
	replaceLeaf(t, document, []string{"datasets", "0", "storage", "sources", "0", "path"}, "inside")
	require.NoError(t, validateObject(root, document, manifestService))
}

func TestDataGovernanceInputBoundaries(t *testing.T) {
	root, document := fixtureManifest(t)
	raw, err := json.Marshal(document)
	require.NoError(t, err)
	raw = append(raw, bytes.Repeat([]byte(" "), (1<<20)-len(raw))...)
	_, err = validateManifest(root, raw, manifestService)
	require.NoError(t, err, "exactly 1 MiB must be accepted")
	_, err = validateManifest(root, append(raw, ' '), manifestService)
	require.Error(t, err)
	for _, depth := range []int{16, 17} {
		var node yaml.Node
		require.NoError(t, yaml.Unmarshal([]byte(strings.Repeat("[", depth-1)+"null"+strings.Repeat("]", depth-1)), &node))
		require.Equal(t, depth == 16, validateYAMLNode(node.Content[0], 1) == nil)
	}
	_, other := fixtureManifest(t)
	second := other["datasets"].([]any)[0].(map[string]any)
	second["id"] = "asset.other"
	document["datasets"] = append(document["datasets"].([]any), second)
	for index := range document["datasets"].([]any) {
		fields := make([]any, 1001)
		for field := range fields {
			fields[field] = map[string]any{"name": fmt.Sprint("field", field), "purpose": "Personal field.", "necessity": "required", "data_classes": []any{"identity"}}
		}
		replaceLeaf(t, document, []string{"datasets", strconv.Itoa(index), "fields"}, fields)
	}
	require.Error(t, validateObject(root, document, manifestService), "field limit applies across datasets")
}
