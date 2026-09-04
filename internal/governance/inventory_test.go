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
	lifecycle := manifestDataset(t, document, "asset.account-purge-lifecycle")
	require.Equal(t, "purged_at < now-180d", lifecycle["retention"].(map[string]any)["rule"].(map[string]any)["predicate"])
	require.Equal(t, "delete", lifecycle["retention"].(map[string]any)["action"])
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
	store, err := os.ReadFile("../postgres/store.go")
	require.NoError(t, err)
	require.Contains(t, string(store), "func (s *Store) CreateUpload")
	require.Contains(t, string(store), "func (s *Store) CreateGrant")
	require.Contains(t, string(store), "owner_id")
	require.Contains(t, string(store), "LEFT JOIN upload_sessions u ON u.asset_id=a.id")
	require.Contains(t, string(store), "SELECT object_key FROM asset_derivatives WHERE asset_id=$1")
	require.Contains(t, string(store), "a.deleted_at IS NOT NULL")
	require.Contains(t, string(store), "u.expires_at < $1")
	require.Contains(t, string(store), "purged_at < $1")
	require.Contains(t, string(store), "LIMIT $2")
	azure, err := os.ReadFile("../storage/azure/store.go")
	require.NoError(t, err)
	require.Contains(t, string(azure), "errors.Is(err, assets.ErrNotFound)")
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
	require.Subset(t, migratedForeignKeys(t), map[string]bool{
		"upload_sessions.asset_id->assets.id":         true,
		"asset_grants.asset_id->assets.id":            true,
		"asset_scan_events.asset_id->assets.id":       true,
		"asset_derivatives.asset_id->assets.id":       true,
		"asset_scan_outbox.asset_id->assets.id":       true,
		"asset_derivative_outbox.asset_id->assets.id": true,
	})
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

func migratedForeignKeys(t *testing.T) map[string]bool {
	t.Helper()
	db := governanceDB(t)
	require.NoError(t, migrations.Run(context.Background(), db))
	rows, err := db.Query(`SELECT kcu.table_name,kcu.column_name,ccu.table_name,ccu.column_name
		FROM information_schema.table_constraints tc
		JOIN information_schema.key_column_usage kcu
		  ON tc.constraint_name=kcu.constraint_name AND tc.table_schema=kcu.table_schema
		JOIN information_schema.constraint_column_usage ccu
		  ON ccu.constraint_name=tc.constraint_name AND ccu.table_schema=tc.table_schema
		WHERE tc.table_schema=current_schema() AND tc.constraint_type='FOREIGN KEY'`)
	require.NoError(t, err)
	defer rows.Close()
	keys := map[string]bool{}
	for rows.Next() {
		var childTable, childColumn, parentTable, parentColumn string
		require.NoError(t, rows.Scan(&childTable, &childColumn, &parentTable, &parentColumn))
		keys[childTable+"."+childColumn+"->"+parentTable+"."+parentColumn] = true
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
