package proxyconfig

import (
	"context"
	"os"
	"testing"
)

func TestLegacyFilesReadOwnerAndDeploymentMappings(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL is required")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TEMP TABLE "LiteLLM_ManagedFileTable" (
		unified_file_id TEXT PRIMARY KEY, storage_backend TEXT, created_by TEXT, team_id TEXT, model_mappings JSONB);
		INSERT INTO "LiteLLM_ManagedFileTable" VALUES
			('fixture',NULL,'owner','team','{"deployment":"file-native"}'),
			('team-fixture','default',NULL,'team','{"deployment":"file-team"}');`)
	if err != nil {
		t.Fatal(err)
	}
	lookup := LegacyFileLookup(db)
	for _, test := range []struct{ key, owner, expected string }{
		{"fixture", "user:owner", "file-native"},
		{"fixture", "team:team", ""},
		{"team-fixture", "team:team", "file-team"},
	} {
		found, err := lookup(context.Background(), test.key, []string{test.owner}, false)
		if test.expected == "" {
			if len(found) > 0 {
				t.Fatal("unauthorized mapping was returned")
			}
		} else if err != nil || found["deployment"] != test.expected {
			t.Fatalf("expected mapping for %s, got %v: %v", test.key, found, err)
		}
	}
}
