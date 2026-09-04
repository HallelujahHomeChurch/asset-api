package governance

import (
	"context"
	"database/sql"
	"fmt"
	"os"
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
	store, err := os.ReadFile("../postgres/store.go")
	require.NoError(t, err)
	require.Contains(t, string(store), "u.expires_at < $1")
	require.Contains(t, string(store), "purged_at < $1")
	require.Contains(t, string(store), "LIMIT $2")
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
	for column := range migratedColumns(t) {
		if _, ok := covered[column]; !ok {
			if reason := operationalColumnExclusions[column]; strings.TrimSpace(reason) == "" {
				t.Errorf("missing data-governance classification for %s", column)
			}
		}
	}
	if len(operationalColumnExclusions) != 3 {
		t.Fatalf("operational exclusions=%v", operationalColumnExclusions)
	}
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

func migratedColumns(t *testing.T) map[string]struct{} {
	t.Helper()
	db := governanceDB(t)
	if err := migrations.Run(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	tables := []string{
		"assets", "upload_sessions", "asset_grants", "asset_scan_events", "asset_derivatives",
		"asset_scan_outbox", "asset_scan_poison_events", "asset_derivative_outbox", "asset_derivative_poison_events",
	}
	rows, err := db.Query(`SELECT table_name,column_name FROM information_schema.columns WHERE table_schema=current_schema() AND table_name = ANY($1) ORDER BY table_name,column_name`, tables)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := map[string]struct{}{}
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatal(err)
		}
		columns[table+"."+column] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(columns) == 0 {
		t.Fatal("migrations exposed no governed columns")
	}
	return columns
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
