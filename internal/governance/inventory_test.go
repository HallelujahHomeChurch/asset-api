package governance

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

const governanceManifestPath = "../../docs/data-governance.yaml"

var operationalColumnExclusions = map[string]string{
	"schema_migrations.version":    "migration version is operational metadata",
	"schema_migrations.checksum":   "migration checksum is operational metadata",
	"schema_migrations.applied_at": "migration application time is operational metadata",
}

var governedTables = map[string]bool{
	"assets":                         true,
	"upload_sessions":                true,
	"asset_grants":                   true,
	"asset_scan_events":              true,
	"asset_derivatives":              true,
	"asset_scan_outbox":              true,
	"asset_scan_poison_events":       true,
	"asset_derivative_outbox":        true,
	"asset_derivative_poison_events": true,
}

func TestDataGovernanceManifest(t *testing.T) {
	raw, err := os.ReadFile(governanceManifestPath)
	require.NoError(t, err)
	document, err := validateManifest("../..", raw, manifestService)
	require.NoError(t, err)
	require.Contains(t, string(raw), "account.avatar and account.dsr-export")
	ids := []string{
		"asset.account-artifact-content", "asset.account-artifact-metadata", "asset.account-upload-sessions",
		"asset.account-grants", "asset.account-scan-and-derivative-state", "asset.account-poison-events", "asset.account-purge-lifecycle",
	}
	require.Equal(t, ids, manifestDatasetIDs(document))
	for _, namespace := range []string{"account.avatar", "account.dsr-export"} {
		policy, ok := assets.PolicyFor(namespace)
		require.Truef(t, ok, "%s policy", namespace)
		require.Equal(t, "account-api", policy.OwnerService)
	}
	upload := manifestDataset(t, document, "asset.account-upload-sessions")
	require.Equal(t, "expires_at < now", upload["retention"].(map[string]any)["rule"].(map[string]any)["predicate"])
	grants := manifestDataset(t, document, "asset.account-grants")
	require.Contains(t, grants["attribution"].(map[string]any)["explanation"], "reader")
	poison := manifestDataset(t, document, "asset.account-poison-events")
	require.Equal(t, "manual", poison["attribution"].(map[string]any)["mode"])
	content := manifestDataset(t, document, "asset.account-artifact-content")
	contentRetention := content["retention"].(map[string]any)
	require.Equal(t, "pending_legal", contentRetention["status"])
	require.Nil(t, contentRetention["rule"])
	require.Equal(t, "none", contentRetention["action"])
	require.Equal(t, "none", content["cleanup"].(map[string]any)["mode"])
	lifecycle := manifestDataset(t, document, "asset.account-purge-lifecycle")
	lifecycleCleanup := lifecycle["cleanup"].(map[string]any)
	lifecycleRetention := lifecycle["retention"].(map[string]any)
	lifecycleRule := lifecycleRetention["rule"].(map[string]any)
	require.Equal(t, "assets.purged_at", lifecycleRule["field"])
	require.Equal(t, "purged_at < now-180d", lifecycleRule["predicate"])
	require.Equal(t, map[string]any{"path": "internal/postgres/store.go", "symbol": "Store.DeleteExpiredPurge"}, lifecycleRule["source"])
	require.Equal(t, map[string]any{"path": "internal/lifecycle/worker.go", "symbol": "Worker.ProcessOne"}, lifecycleRule["duration_source"])
	require.Equal(t, "existing_worker", lifecycleCleanup["mode"])
	require.Equal(t, map[string]bool{
		"internal/assets/service.go:Service.SoftDelete":       true,
		"internal/lifecycle/worker.go:Worker.ProcessOne":      true,
		"internal/storage/azure/store.go:Store.Delete":        true,
		"internal/postgres/store.go:Store.DeleteExpiredPurge": true,
	}, manifestReferences(t, lifecycleCleanup["implementation"]))
	require.Equal(t, map[string]bool{
		"internal/assets/service_test.go:TestSoftDeleteImmediatelyBlocksPublicDownload":                               true,
		"internal/lifecycle/worker_test.go:TestWorkerRetriesBlobFailure":                                              true,
		"internal/storage/azure/store_test.go:TestDeleteMissingBlobIsRepeatSafe":                                      true,
		"internal/postgres/store_integration_test.go:TestDeleteExpiredPurgeIsBoundedAndPreservesRecentOrActiveAssets": true,
	}, manifestEvidence(t, lifecycleCleanup["evidence"]))
	require.Equal(t, "delete", lifecycleRetention["action"])
	nonActivityFields := 0
	for _, value := range document["datasets"].([]any) {
		for _, value := range value.(map[string]any)["fields"].([]any) {
			field := value.(map[string]any)
			require.NotEqual(t, "Governed field", field["purpose"], "field=%s", field["name"])
			if !reflect.DeepEqual([]any{"activity"}, field["data_classes"]) {
				nonActivityFields++
			}
		}
	}
	require.Greater(t, nonActivityFields, 0)
	uploadWriter := goFunctionRegion(t, "../postgres/store.go", "func (s *Store) CreateUpload")
	require.Contains(t, uploadWriter, "owner_id")
	grantWriter := goFunctionRegion(t, "../postgres/store.go", "func (s *Store) CreateGrant")
	require.Contains(t, grantWriter, "asset_id")
	claimPurge := goFunctionRegion(t, "../postgres/store.go", "func (s *Store) ClaimPurge")
	require.Contains(t, claimPurge, "LEFT JOIN upload_sessions u ON u.asset_id=a.id")
	require.Contains(t, claimPurge, "SELECT object_key FROM asset_derivatives WHERE asset_id=$1")
	require.Contains(t, claimPurge, "a.deleted_at IS NOT NULL")
	require.Contains(t, claimPurge, "u.expires_at < $1")
	deleteExpiredPurge := goFunctionRegion(t, "../postgres/store.go", "func (s *Store) DeleteExpiredPurge")
	require.Contains(t, deleteExpiredPurge, "purged_at < $1")
	require.Contains(t, deleteExpiredPurge, "LIMIT $2")
	softDelete := goFunctionRegion(t, "../assets/service.go", "func (s *Service) SoftDelete")
	require.Contains(t, softDelete, "SoftDeleteAsset")
	azureDelete := goFunctionRegion(t, "../storage/azure/store.go", "func (s *Store) Delete")
	require.Contains(t, azureDelete, "errors.Is(err, assets.ErrNotFound)")
	bicep, err := os.ReadFile("../../infra/main.bicep")
	require.NoError(t, err)
	require.Contains(t, string(bicep), "param retentionScheduleEnabled bool = false")
	require.Contains(t, string(bicep), "param retentionApplyEnabled bool = false")
}

func manifestDatasetIDs(document map[string]any) []string {
	values := document["datasets"].([]any)
	ids := make([]string, 0, len(values))
	for _, value := range values {
		ids = append(ids, value.(map[string]any)["id"].(string))
	}
	return ids
}

func manifestDataset(t *testing.T, document map[string]any, id string) map[string]any {
	t.Helper()
	for _, value := range document["datasets"].([]any) {
		dataset := value.(map[string]any)
		if dataset["id"] == id {
			return dataset
		}
	}
	t.Fatalf("missing dataset %s", id)
	return nil
}

func TestDataGovernanceMigratedColumnCoverage(t *testing.T) {
	raw, err := os.ReadFile(governanceManifestPath)
	require.NoError(t, err)
	document, err := validateManifest("../..", raw, manifestService)
	require.NoError(t, err)

	covered := manifestFieldNames(t, document)
	columns, tables := migratedColumns(t)
	require.Equal(t, governedTables, tables)
	for column := range columns {
		if _, ok := covered[column]; !ok {
			if reason := operationalColumnExclusions[column]; strings.TrimSpace(reason) == "" {
				t.Errorf("missing data-governance classification for %s", column)
			}
		}
	}
	if len(operationalColumnExclusions) != 3 {
		t.Fatalf("operational exclusions=%v", operationalColumnExclusions)
	}
	foreignKeys := migratedAssetForeignKeyDeleteRules(t)
	require.NotContains(t, foreignKeys, "asset_scan_poison_events.asset_id->assets.id")
	require.NotContains(t, foreignKeys, "asset_derivative_poison_events.asset_id->assets.id")
	require.Equal(t, map[string]string{
		"upload_sessions.asset_id->assets.id":         "c",
		"asset_grants.asset_id->assets.id":            "c",
		"asset_scan_events.asset_id->assets.id":       "c",
		"asset_derivatives.asset_id->assets.id":       "c",
		"asset_scan_outbox.asset_id->assets.id":       "c",
		"asset_derivative_outbox.asset_id->assets.id": "c",
	}, foreignKeys)
}

func manifestReferences(t *testing.T, values any) map[string]bool {
	t.Helper()
	result := map[string]bool{}
	for _, value := range values.([]any) {
		reference := value.(map[string]any)
		result[reference["path"].(string)+":"+reference["symbol"].(string)] = true
	}
	return result
}

func manifestEvidence(t *testing.T, values any) map[string]bool {
	t.Helper()
	result := map[string]bool{}
	for _, value := range values.([]any) {
		evidence := value.(map[string]any)
		result[evidence["path"].(string)+":"+evidence["test_name"].(string)] = true
	}
	return result
}

func goFunctionRegion(t *testing.T, path, signature string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	start := strings.Index(string(raw), signature)
	require.NotEqual(t, -1, start, "missing %s in %s", signature, path)
	region := string(raw[start:])
	if next := strings.Index(region[1:], "\nfunc "); next >= 0 {
		return region[:next+1]
	}
	return region
}

func manifestFieldNames(t *testing.T, document map[string]any) map[string]struct{} {
	t.Helper()
	covered := map[string]struct{}{}
	for _, value := range document["datasets"].([]any) {
		dataset := value.(map[string]any)
		for _, value := range dataset["fields"].([]any) {
			covered[value.(map[string]any)["name"].(string)] = struct{}{}
		}
	}
	return covered
}

func migratedColumns(t *testing.T) (map[string]struct{}, map[string]bool) {
	t.Helper()
	db := governanceDB(t)
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	tableNames := []string{
		"assets", "upload_sessions", "asset_grants", "asset_scan_events", "asset_derivatives",
		"asset_scan_outbox", "asset_scan_poison_events", "asset_derivative_outbox", "asset_derivative_poison_events",
	}
	rows, err := db.Query(`SELECT table_name,column_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name = ANY($1) ORDER BY table_name,column_name`, tableNames)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := map[string]struct{}{}
	observedTables := map[string]bool{}
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatal(err)
		}
		columns[table+"."+column] = struct{}{}
		observedTables[table] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(columns) == 0 {
		t.Fatal("migrations exposed no governed columns")
	}
	return columns, observedTables
}

func migratedAssetForeignKeyDeleteRules(t *testing.T) map[string]string {
	t.Helper()
	db := governanceDB(t)
	require.NoError(t, migrations.Run(context.Background(), db))
	rows, err := db.Query(`SELECT child.relname,child_column.attname,parent.relname,parent_column.attname,c.confdeltype
		FROM pg_constraint c
		JOIN pg_class child ON child.oid=c.conrelid
		JOIN pg_namespace child_schema ON child_schema.oid=child.relnamespace
		JOIN pg_class parent ON parent.oid=c.confrelid
		JOIN pg_attribute child_column ON child_column.attrelid=c.conrelid AND child_column.attnum=c.conkey[1]
		JOIN pg_attribute parent_column ON parent_column.attrelid=c.confrelid AND parent_column.attnum=c.confkey[1]
		WHERE c.contype='f' AND child_schema.nspname=current_schema()
		  AND child.relname = ANY($1)`, []string{
		"upload_sessions", "asset_grants", "asset_scan_events", "asset_derivatives", "asset_scan_outbox", "asset_derivative_outbox",
		"asset_scan_poison_events", "asset_derivative_poison_events",
	})
	require.NoError(t, err)
	defer rows.Close()
	keys := map[string]string{}
	for rows.Next() {
		var childTable, childColumn, parentTable, parentColumn, deleteRule string
		require.NoError(t, rows.Scan(&childTable, &childColumn, &parentTable, &parentColumn, &deleteRule))
		keys[childTable+"."+childColumn+"->"+parentTable+"."+parentColumn] = deleteRule
	}
	require.NoError(t, rows.Err())
	return keys
}

func governanceDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL is required")
	}
	config, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	admin := stdlib.OpenDB(*config)
	t.Cleanup(func() { admin.Close() })
	schema := fmt.Sprintf("asset_governance_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	config = config.Copy()
	config.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestDataGovernanceExclusionsAreOperationalMetadata(t *testing.T) {
	columns := make([]string, 0, len(operationalColumnExclusions))
	for column := range operationalColumnExclusions {
		columns = append(columns, column)
	}
	sort.Strings(columns)
	require.Equal(t, []string{"schema_migrations.applied_at", "schema_migrations.checksum", "schema_migrations.version"}, columns)
}
