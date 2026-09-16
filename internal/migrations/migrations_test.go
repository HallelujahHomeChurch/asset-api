package migrations

import (
	"strings"
	"testing"
)

func TestProtectBulletinAssetsMigrationRevokesPublicGrantsAndVisibility(t *testing.T) {
	contents, err := files.ReadFile("sql/023_protect_bulletin_assets.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(contents)
	for _, expected := range []string{"a.namespace = 'cms.weekly.pdf'", "g.subject_type = 'public'", "g.revoked_at IS NULL", "SET visibility = 'private'"} {
		if !strings.Contains(sql, expected) {
			t.Fatalf("migration missing %q", expected)
		}
	}
}
