package migrations

import (
	"regexp"
	"strings"
	"testing"
)

func TestAssetCollectionMigrationPolicy(t *testing.T) {
	contents, err := files.ReadFile("sql/012_asset_collections.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := strings.ToLower(string(contents))
	for name, pattern := range map[string]string{
		"stable item occurrence":   `create table[^;]+asset_collection_items[^;]+id text[^;]+created_revision[^;]+deleted_revision`,
		"active asset uniqueness":  `unique index[^;]+asset_collection_items[^;]+collection_id\s*,\s*asset_id[^;]+where[^;]+deleted_revision is null`,
		"active remote uniqueness": `unique index[^;]+asset_collection_items[^;]+collection_id\s*,\s*remote_item_id[^;]+where[^;]+deleted_revision is null`,
		"asset retention":          `asset_id text references assets\s*\(id\) on delete set null`,
		"nullable mutation claim":  `response_json jsonb`,
		"ticket item occurrence":   `collection_item_id text not null references asset_collection_items\s*\(id\)`,
		"ticket role snapshot":     `roles text\[\] not null`,
	} {
		if !regexp.MustCompile(pattern).MatchString(sql) {
			t.Errorf("missing %s policy", name)
		}
	}
}
