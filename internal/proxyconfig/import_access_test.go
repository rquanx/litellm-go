package proxyconfig

import (
	"os"
	"slices"
	"testing"
)

func TestPostgresImportsBoundLiteLLMKeysAndMemberScopes(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL is unset")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
		CREATE TEMP TABLE minato_proxy_state (name TEXT PRIMARY KEY, revision BIGINT NOT NULL, value JSONB NOT NULL);
		CREATE TEMP TABLE "LiteLLM_BudgetTable" (budget_id TEXT, max_budget REAL, allowed_models TEXT[]);
		CREATE TEMP TABLE "LiteLLM_OrganizationTable" (organization_id TEXT, organization_alias TEXT, models TEXT[], budget_id TEXT);
		CREATE TEMP TABLE "LiteLLM_UserTable" (user_id TEXT, user_role TEXT, models TEXT[]);
		CREATE TEMP TABLE "LiteLLM_ObjectPermissionTable" (object_permission_id TEXT, search_tools TEXT[], mcp_servers TEXT[]);
		CREATE TEMP TABLE "LiteLLM_TeamTable" (team_id TEXT, organization_id TEXT, models TEXT[], members_with_roles JSONB, object_permission_id TEXT);
		CREATE TEMP TABLE "LiteLLM_TeamMembership" (team_id TEXT, user_id TEXT, budget_id TEXT);
		CREATE TEMP TABLE "LiteLLM_OrganizationMembership" (organization_id TEXT, user_id TEXT, user_role TEXT);
		CREATE TEMP TABLE "LiteLLM_VerificationToken" (token TEXT, user_id TEXT, team_id TEXT, models TEXT[], created_at TIMESTAMP, object_permission_id TEXT);
		INSERT INTO "LiteLLM_BudgetTable" VALUES ('org-budget', NULL, ARRAY[]::TEXT[]), ('member-budget', 1, ARRAY['allowed']);
		INSERT INTO "LiteLLM_OrganizationTable" VALUES ('org', 'Fixture', ARRAY['allowed','org-only'], 'org-budget');
		INSERT INTO "LiteLLM_UserTable" VALUES ('user', 'internal_user', ARRAY['allowed','org-only','user-only']);
		INSERT INTO "LiteLLM_ObjectPermissionTable" VALUES
			('team-permission', ARRAY['shared','team-search'], NULL), ('key-permission', ARRAY['shared','key-search'], ARRAY['unsupported']);
		INSERT INTO "LiteLLM_TeamTable" VALUES ('team', 'org', ARRAY['allowed','org-only','team-only'], '[{"user_id":"user","role":"user"}]', 'team-permission');
		INSERT INTO "LiteLLM_TeamMembership" VALUES ('team', 'user', 'member-budget');
		INSERT INTO "LiteLLM_OrganizationMembership" VALUES ('org', 'user', 'internal_user');
	`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO "LiteLLM_VerificationToken" VALUES ($1,'user','team',ARRAY['all-team-models'],now(),'key-permission')`, keyDigest("fixture-token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ImportLiteLLMAccess(db); err == nil {
		t.Fatal("an active monetary budget was silently dropped")
	}
	var rows int
	if err := db.QueryRow("SELECT count(*) FROM minato_proxy_state").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("failed import wrote access state: rows=%d, err=%v", rows, err)
	}
	if _, err := db.Exec(`UPDATE "LiteLLM_BudgetTable" SET max_budget = NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportLiteLLMAccess(db); err == nil {
		t.Fatal("unsupported MCP restrictions must block migration")
	}
	if _, err := db.Exec(`UPDATE "LiteLLM_ObjectPermissionTable" SET mcp_servers = NULL`); err != nil {
		t.Fatal(err)
	}
	if count, err := ImportLiteLLMAccess(db); err != nil || count != 1 {
		t.Fatalf("count=%d, err=%v", count, err)
	}
	registry, err := LoadPostgresKeys("master", db)
	if err != nil {
		t.Fatal(err)
	}
	if scope, allowed := registry.Authorize("fixture-token"); !allowed || !slices.Equal(scope, []string{"allowed"}) {
		t.Fatalf("restored scope=%v, allowed=%v", scope, allowed)
	}
	if scope, allowed := registry.AuthorizeSearch("fixture-token"); !allowed || !slices.Equal(scope, []string{"shared"}) {
		t.Fatalf("restored search scope=%v, allowed=%v", scope, allowed)
	}
}
