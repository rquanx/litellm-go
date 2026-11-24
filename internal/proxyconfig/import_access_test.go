package proxyconfig

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rquanx/litellm-go/internal/runtime"
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
		CREATE TEMP TABLE "LiteLLM_BudgetTable" (budget_id TEXT, max_budget REAL, allowed_models TEXT[], rpm_limit INTEGER, tpm_limit INTEGER);
		ALTER TABLE "LiteLLM_BudgetTable" ADD COLUMN max_parallel_requests INTEGER;
		CREATE TEMP TABLE "LiteLLM_OrganizationTable" (organization_id TEXT, organization_alias TEXT, models TEXT[], budget_id TEXT, spend DOUBLE PRECISION);
		CREATE TEMP TABLE "LiteLLM_UserTable" (user_id TEXT, user_role TEXT, models TEXT[],
			max_budget DOUBLE PRECISION, spend DOUBLE PRECISION, budget_duration TEXT, budget_reset_at TIMESTAMP);
		CREATE TEMP TABLE "LiteLLM_ObjectPermissionTable" (object_permission_id TEXT, search_tools TEXT[], mcp_servers TEXT[], mcp_toolsets TEXT[], agents TEXT[]);
		CREATE TEMP TABLE "LiteLLM_TeamTable" (team_id TEXT, organization_id TEXT, models TEXT[], members_with_roles JSONB, object_permission_id TEXT,
			max_budget DOUBLE PRECISION, spend DOUBLE PRECISION, metadata JSONB);
		CREATE TEMP TABLE "LiteLLM_TeamMembership" (team_id TEXT, user_id TEXT, budget_id TEXT, spend DOUBLE PRECISION);
		CREATE TEMP TABLE "LiteLLM_ProjectTable" (project_id TEXT, team_id TEXT, models TEXT[], budget_id TEXT, spend DOUBLE PRECISION);
		CREATE TEMP TABLE "LiteLLM_OrganizationMembership" (organization_id TEXT, user_id TEXT, user_role TEXT);
		CREATE TEMP TABLE "LiteLLM_VerificationToken" (token TEXT, user_id TEXT, team_id TEXT, models TEXT[], created_at TIMESTAMP, object_permission_id TEXT, project_id TEXT);
		INSERT INTO "LiteLLM_BudgetTable" (budget_id, max_budget, allowed_models, rpm_limit) VALUES
			('org-budget', NULL, ARRAY[]::TEXT[], NULL), ('member-budget', 1, ARRAY['allowed'], 10),
			('default-member-budget', 0.75, ARRAY[]::TEXT[], NULL), ('project-budget', 2, ARRAY[]::TEXT[], NULL);
		INSERT INTO "LiteLLM_OrganizationTable" VALUES ('org', 'Fixture', ARRAY['allowed','org-only'], 'org-budget', 1.5);
		INSERT INTO "LiteLLM_UserTable" VALUES ('user', 'internal_user', ARRAY['allowed','org-only','user-only'],
			0.5, 0.5, '1d', (CURRENT_TIMESTAMP AT TIME ZONE 'UTC') + interval '1 day');
		INSERT INTO "LiteLLM_ObjectPermissionTable" VALUES
			('team-permission', ARRAY['shared','team-search'], ARRAY['fixture-mcp'], ARRAY['toolset'], NULL),
			('key-permission', ARRAY['shared','key-search'], NULL, NULL, ARRAY['fixture-agent']);
		INSERT INTO "LiteLLM_TeamTable" VALUES ('team', 'org', ARRAY['allowed','org-only','team-only'], '[{"user_id":"user","role":"user"}]',
			'team-permission', 1, 0.5, '{"team_member_budget_id":"default-member-budget"}');
		INSERT INTO "LiteLLM_TeamMembership" VALUES ('team', 'user', 'member-budget', 1);
		INSERT INTO "LiteLLM_ProjectTable" VALUES ('project','team',ARRAY['allowed'],'project-budget', 0.75);
		INSERT INTO "LiteLLM_OrganizationMembership" VALUES ('org', 'user', 'internal_user');
		ALTER TABLE "LiteLLM_TeamTable" ADD COLUMN router_settings JSONB;
		UPDATE "LiteLLM_TeamTable" SET router_settings = '{"num_retries":1}';
		ALTER TABLE "LiteLLM_VerificationToken" ADD COLUMN router_settings TEXT;
		ALTER TABLE "LiteLLM_UserTable" ADD COLUMN password TEXT;
		CREATE TEMP TABLE "LiteLLM_InvitationLink" (id TEXT, user_id TEXT, is_accepted BOOLEAN, accepted_at TIMESTAMP,
			expires_at TIMESTAMP, created_at TIMESTAMP, created_by TEXT, updated_at TIMESTAMP, updated_by TEXT);
		INSERT INTO "LiteLLM_InvitationLink" VALUES ('fixture-invite','user',FALSE,NULL,
			(CURRENT_TIMESTAMP AT TIME ZONE 'UTC') + interval '1 day',
			CURRENT_TIMESTAMP AT TIME ZONE 'UTC','admin',CURRENT_TIMESTAMP AT TIME ZONE 'UTC','admin');
	`)
	if err != nil {
		t.Fatal(err)
	}
	fixtureHash := keyDigest("fixture-imported-password")
	scryptFixture := "scrypt:AAECAwQFBgcICQoLDA0OD5u8cYkTC7ltZfws0fKB0pejhJoHMtB6ffENSLVZE3Ht"
	if _, err := db.Exec(`UPDATE "LiteLLM_UserTable" SET password=$1 WHERE user_id='user'`, fixtureHash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO "LiteLLM_UserTable" (user_id,user_role,models,password) VALUES ('other','internal_user',ARRAY[]::TEXT[],$1)`, scryptFixture); err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO "LiteLLM_VerificationToken" VALUES ($1,'user','team',ARRAY['all-team-models'],now(),'key-permission','project','num_retries: 2')`, keyDigest("fixture-access-token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TEMP TABLE "LiteLLM_AccessGroupTable" (access_group_id TEXT, access_group_name TEXT,
			created_at TIMESTAMP, updated_at TIMESTAMP, access_model_names TEXT[], access_agent_ids TEXT[]);
		INSERT INTO "LiteLLM_AccessGroupTable" VALUES ('group','Fixture Group',now(),now(),ARRAY['org-only'],ARRAY['group-agent']);
		ALTER TABLE "LiteLLM_TeamTable" ADD COLUMN access_group_ids TEXT[];
		ALTER TABLE "LiteLLM_VerificationToken" ADD COLUMN access_group_ids TEXT[];
		UPDATE "LiteLLM_VerificationToken" SET access_group_ids = ARRAY['group'];
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportLiteLLMAccess(db); err == nil {
		t.Fatal("an unsupported request limit was silently dropped")
	}
	var rows int
	if err := db.QueryRow("SELECT count(*) FROM minato_proxy_state").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("failed import wrote access state: rows=%d, err=%v", rows, err)
	}
	if _, err := db.Exec(`UPDATE "LiteLLM_BudgetTable" SET rpm_limit = NULL`); err != nil {
		t.Fatal(err)
	}

	var source *redis.Client
	if redisURL := os.Getenv("MINATO_TEST_REDIS_URL"); redisURL != "" {
		options, err := redis.ParseURL(redisURL)
		if err != nil {
			t.Fatal(err)
		}
		source = redis.NewClient(options)
		defer source.Close()
		if _, err := db.Exec(`
			ALTER TABLE "LiteLLM_TeamTable" ADD COLUMN budget_limits TEXT;
			ALTER TABLE "LiteLLM_VerificationToken" ADD COLUMN budget_limits JSONB;
			UPDATE "LiteLLM_TeamTable" SET budget_limits = '[{"budget_duration":"7d","max_budget":3,"reset_at":"2099-01-01T00:00:00"}]';
			UPDATE "LiteLLM_VerificationToken" SET budget_limits = '[{"budget_duration":"1d","max_budget":2,"reset_at":"2099-01-01T00:00:00Z"}]';
		`); err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		keyCounter := "fixture:spend:key:" + keyDigest("fixture-access-token") + ":window:1d"
		teamCounter := "fixture:spend:team:team:window:7d"
		if _, err := ImportLiteLLMAccess(db); err == nil || !strings.Contains(err.Error(), "IMPORT_REDIS_URL") {
			t.Fatalf("expected an explicit Redis source requirement: %v", err)
		}
		if err := source.MSet(ctx, keyCounter, "invalid", teamCounter, "3.5").Err(); err != nil {
			t.Fatal(err)
		}
		if _, err := ImportLiteLLMAccessWithRedis(db, source, "fixture"); err == nil || !strings.Contains(err.Error(), "invalid source Redis") {
			t.Fatalf("expected an invalid-counter error: %v", err)
		}
		if err := source.Set(ctx, keyCounter, "2.25", 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	if count, err := ImportLiteLLMAccessWithRedis(db, source, "fixture"); err != nil || count != 1 {
		t.Fatalf("count=%d, err=%v", count, err)
	}
	registry, err := LoadPostgresKeys("master", db)
	if err != nil {
		t.Fatal(err)
	}
	if !verifyDashboardPassword("fixture-imported-password", registry.state.Passwords["user"]) ||
		verifyDashboardPassword(fixtureHash, registry.state.Passwords["user"]) {
		t.Fatal("the imported SHA-256 hash did not preserve password authentication")
	}
	if !verifyDashboardPassword("fixture-imported-password", registry.state.Passwords["other"]) {
		t.Fatal("the imported Python scrypt hash did not preserve password authentication")
	}
	invite := registry.state.Invitations["fixture-invite"]
	if invite.UserID != "user" || invite.Accepted || invite.AcceptedAt != nil || !invite.ExpiresAt.After(invite.CreatedAt) {
		t.Fatal("the imported invitation lost its owner, expiration or acceptance state")
	}
	if scope, allowed := registry.Authorize("fixture-access-token"); !allowed || !slices.Equal(scope, []string{"allowed"}) {
		t.Fatalf("restored scope=%v, allowed=%v", scope, allowed)
	}
	importedKey := registry.state.Keys[keyDigest("fixture-access-token")]
	group := registry.state.AccessGroups["group"]
	if !slices.Equal(importedKey.AccessGroupIDs, []string{"group"}) ||
		!slices.Equal(importedKey.RestrictedGroupIDs, []string{"group"}) ||
		group.Name != "Fixture Group" || group.CreatedAt.IsZero() {
		t.Fatal("import changed the source access group ownership")
	}
	if grants := registry.AuthorizeAgent("fixture-access-token"); grants == nil || !grants(runtime.Agent{ID: "group-agent"}) {
		t.Fatal("imported access group did not grant agent access")
	}
	spend, err := OpenSpendLog(context.Background(), db, registry)
	if err != nil {
		t.Fatal(err)
	}
	registry.state.Keys[keyDigest("fixture-personal")] = virtualKey{UserID: "user"}
	personal, err := spend.CheckKeyBudget(context.Background(), "fixture-personal")
	if err != nil || personal.ErrorMessage == "" {
		t.Fatalf("expected the imported user balance to block personal inference: %+v, %v", personal, err)
	}
	bound, err := spend.CheckKeyBudget(context.Background(), "fixture-access-token")
	if err != nil || !strings.Contains(bound.ErrorMessage, "ExceededBudget: Team member") {
		t.Fatalf("expected the imported member cap to block the team project key: %+v, %v", bound, err)
	}
	entry := registry.state.Teams["team"]
	if entry.RouterSettings["num_retries"] != float64(1) || bound.RouterSettings["num_retries"] != float64(2) {
		t.Fatalf("router settings did not survive import: team=%v, key=%v", entry.RouterSettings, bound.RouterSettings)
	}
	if entry.MaxBudget == nil || *entry.MaxBudget != 1 || entry.ImportedSpend == nil || entry.ImportedSpend.Amount != 0.5 {
		t.Fatal("team cap and recorded balance must survive import")
	}
	if entry.MemberBudget == nil || *entry.MemberBudget != 0.75 || *entry.memberBudget("user").MaxBudget != 1 {
		t.Fatal("team member default and override must survive import")
	}
	project := registry.state.Projects["project"]
	org := registry.state.Organizations["org"]
	if project.BudgetID != "project-budget" || project.ImportedSpend == nil || project.ImportedSpend.Amount != 0.75 ||
		org.BudgetID != "org-budget" || org.ImportedSpend == nil || org.ImportedSpend.Amount != 1.5 {
		t.Fatal("project and organization budgets must retain the tier ID and spend balance")
	}
	if scope, allowed := registry.AuthorizeSearch("fixture-access-token"); !allowed || !slices.Equal(scope, []string{"shared"}) {
		t.Fatalf("restored search scope=%v, allowed=%v", scope, allowed)
	}
	if access, allowed := registry.AuthorizeMCP("fixture-access-token"); !allowed ||
		!access(runtime.MCPServer{ID: "fixture-mcp"}, "") ||
		access(runtime.MCPServer{ID: "other-mcp"}, "") {
		t.Fatal("imported key did not inherit the team's MCP server grant")
	}
	if grants := registry.AuthorizeAgent("fixture-access-token"); grants == nil ||
		!grants(runtime.Agent{ID:"fixture-agent"}) || grants(runtime.Agent{ID:"other-agent",Public:true}) {
		t.Fatal("imported key did not preserve its agent grant")
	}
	if grants, _, valid := registry.AuthorizeMCPToolset("fixture-access-token"); !valid || !slices.Equal(grants, []string{"toolset"}) {
		t.Fatal("imported key did not inherit the team's MCP toolset grant")
	}
	if source != nil {
		digest := keyDigest("fixture-access-token")
		entry.MemberBudget = nil
		entry.Members[0].MaxBudget = nil
		registry.state.Teams["team"] = entry
		if result, err := spend.CheckKeyBudget(context.Background(), "fixture-access-token"); err != nil ||
			!strings.Contains(result.ErrorMessage, "Team=team over 7d budget. Spend=$3.5000") {
			t.Fatalf("team window balance was not enforced: %v %v", result.ErrorMessage, err)
		}
		entry.BudgetLimits = nil
		registry.state.Teams["team"] = entry
		window := registry.state.Keys[digest].BudgetLimits[0]
		for _, charge := range []struct {
			at time.Time
			cost float64
		}{{window.ImportedSpend.AsOf.Add(-time.Second), 8}, {window.ImportedSpend.AsOf.Add(time.Second), 0.25}} {
			start := float64(charge.at.UnixMicro()) / 1e6
			spend.Record(runtime.InferenceEvent{ID: newEntityID(), Model: "allowed", KeyDigest: digest,
				StartTime: start, EndTime: start+0.1, Spend: &charge.cost})
		}
		if result, err := spend.CheckKeyBudget(context.Background(), "fixture-access-token"); err != nil ||
			!strings.Contains(result.ErrorMessage, "Key over 1d budget. Spend=$2.5000") {
			t.Fatalf("key window balance was not enforced: %v %v", result.ErrorMessage, err)
		}
		_, afterReset := registry.budgetLimitWindows([]keyBudgetLimit{window}, *window.ResetAt)
		if afterReset[0] != 0 {
			t.Fatal("imported balance survived its first reset")
		}
	}
}
