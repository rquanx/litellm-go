package proxyconfig

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestPostgresSpendLogsAndCostTrend(t *testing.T) {
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
	if _, err := db.Exec("CREATE TEMP TABLE minato_proxy_spend (request_id TEXT PRIMARY KEY, started_at TIMESTAMPTZ NOT NULL, ended_at TIMESTAMPTZ NOT NULL," +
		" api_key TEXT, key_alias TEXT, user_id TEXT, team_id TEXT, model TEXT, model_group TEXT, provider TEXT, call_type TEXT," +
		" status TEXT, prompt_tokens BIGINT, completion_tokens BIGINT, total_tokens BIGINT, spend DOUBLE PRECISION)"); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	digest := keyDigest("sk-test")
	keys.state.Projects = map[string]project{"fixture-project": {ID: "fixture-project", TeamID: "fixture-team"}}
	keys.state.Teams["fixture-team"] = team{ID: "fixture-team", Alias: "fixture alias"}
	keys.state.Keys[digest] = virtualKey{Alias: "test-key", ProjectID: "fixture-project"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	spend, err := OpenSpendLog(ctx, db, keys)
	if err != nil {
		t.Fatal(err)
	}
	cost := 0.01
	now := float64(time.Now().Add(-time.Second).Unix())
	spend.Record(runtime.InferenceEvent{ID: "req-one", KeyDigest: digest, Model: "fixture", ModelGroup: "demo",
		StartTime: now, EndTime: now, Status: "success", Total: 3, Spend: &cost})
	spend.Record(runtime.InferenceEvent{ID: "req-two", Model: "unpriced", ModelGroup: "demo",
		StartTime: now, EndTime: now, Status: "success", Total: 2})
	handler := spend.Handler(http.NotFoundHandler())
	call := func(token, path string, status int, content string) string {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != status || !strings.Contains(response.Body.String(), content) {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body)
		}
		return response.Body.String()
	}
	call("sk-test", "/spend/logs", 401, "master key")
	found := call("master", "/spend/logs/v2?api_key=sk-test", 200, `"total_count":1`)
	if !strings.Contains(found, "test-key") || !strings.Contains(found, digest) || strings.Contains(found, "sk-test") {
		t.Fatalf("unexpected spend log attribution: %s", found)
	}
	call("master", "/spend/logs?request_id=req-two", 200, `"spend":null`)
	call("master", "/spend/logs/v2?project_id=fixture-project", 200, `"total_count":1`)
	call("master", "/spend/logs/v2?team_id=fixture-team", 200, `"total_count":1`)
	call("master", "/global/spend/logs", 200, `"total_tokens":5`)
	call("master", "/global/spend/models?limit=1", 200, `"model":"fixture"`)
	call("master", "/global/spend/keys?limit=1", 200, `"key_alias":"test-key"`)
	call("master", "/global/spend/teams", 200, `"fixture alias":0.01`)
	if _, err := db.Exec(`CREATE TEMP TABLE "LiteLLM_SpendLogs" (
		request_id TEXT, "startTime" TIMESTAMP, "endTime" TIMESTAMP, api_key TEXT, metadata JSONB,
		"user" TEXT, team_id TEXT, model TEXT, model_group TEXT, model_id TEXT, custom_llm_provider TEXT,
		call_type TEXT, status TEXT, prompt_tokens INT, completion_tokens INT, total_tokens INT, spend DOUBLE PRECISION);
		INSERT INTO "LiteLLM_SpendLogs" VALUES ('historical', '2026-01-01 12:00:00', '2026-01-01 12:00:01',
		'sk-plaintext-fixture', '{"project_id":"imported"}','fixture-user','fixture-team','fixture-model','fixture-group',
		'fixture-id','openai','acompletion','success',2,3,5,0.015);`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE "LiteLLM_SpendLogs" ADD COLUMN cache_hit TEXT DEFAULT 'True', ADD COLUMN agent_id TEXT DEFAULT 'historical-agent'`); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []int64{1, 0} {
		if count, err := ImportLiteLLMSpend(db); err != nil || count != expected {
			t.Fatalf("imported %d, want %d: %v", count, expected, err)
		}
	}
	var agentID string
	if err := db.QueryRow(`SELECT agent_id FROM minato_proxy_spend WHERE request_id = 'historical'`).Scan(&agentID); err != nil || agentID != "historical-agent" {
		t.Fatalf("could not restore historical agent attribution: %q %v", agentID, err)
	}
	historical := call("master", "/spend/logs/v2?project_id=imported", 200, `"total_count":1`)
	if strings.Contains(historical, "sk-plaintext-fixture") || !strings.Contains(historical, `"total_tokens":5`) {
		t.Fatalf("unexpected historical spend %s", historical)
	}
	keys.state.Users = map[string]internalUser{"fixture-user": {ID: "fixture-user", Role: "internal_user"}}
	owner := keys.state.Teams["fixture-team"]
	owner.Members = []teamMembership{{TeamID: owner.ID, UserID: "fixture-user", Role: "user"}}
	keys.state.Teams[owner.ID] = owner
	keys.state.Keys[keyDigest("member")] = virtualKey{UserID: "fixture-user"}
	keys.SetTeamMemberSpend(spend)
	handler = keys.Handler(handler)
	call("member", "/team/fixture-team/members/me", 200, `"total_spend":0.015`)
	call("member", "/team/other/members/me", 404, "not found")
	call("sk-test", "/team/fixture-team/members/me", 400, "no associated user_id")
	activity := "/global/activity/cache_hits?start_date=2026-01-01&end_date=2026-01-01"
	call("sk-test", activity, 401, "master key")
	call("master", activity, 200, `"cache_hit_true_rows":1,"cached_completion_tokens":3,"generated_completion_tokens":0`)
}

func TestAgentDailyActivityIsScoped(t *testing.T) {
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
	_, err = db.Exec(`CREATE TEMP TABLE minato_proxy_spend (
		request_id TEXT PRIMARY KEY, started_at TIMESTAMPTZ NOT NULL, ended_at TIMESTAMPTZ NOT NULL, api_key TEXT NOT NULL,
		key_alias TEXT NOT NULL, user_id TEXT NOT NULL, team_id TEXT NOT NULL, model TEXT NOT NULL, model_group TEXT NOT NULL,
		provider TEXT NOT NULL, call_type TEXT NOT NULL, status TEXT NOT NULL, prompt_tokens BIGINT NOT NULL,
		completion_tokens BIGINT NOT NULL,total_tokens BIGINT NOT NULL,spend DOUBLE PRECISION)`)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	keys.state.Keys[keyDigest("scoped")] = virtualKey{ObjectPermission: &objectPermission{Agents: []string{"allowed"}}}
	log, err := OpenSpendLog(context.Background(), db, keys)
	if err != nil {
		t.Fatal(err)
	}
	now := float64(time.Now().Unix())
	for _, id := range []string{"allowed", "private"} {
		log.Record(runtime.InferenceEvent{ID: id, StartTime: now - 1, EndTime: now, Model: "a2a_agent/" + id, ModelID: id, Provider: "a2a_agent",
			Status: "success", CallType: "asend_message", Total: 4, Prompt: 2, Completion: 2})
	}
	server := runtime.NewServer(runtime.Config{AuthorizeAgent: keys.AuthorizeAgent, Agents: []runtime.Agent{{ID: "allowed"}, {ID: "private", Public: true}}})
	handler := log.Handler(http.NotFoundHandler(), server)
	day := time.Now().UTC().Format(time.DateOnly)
	req := httptest.NewRequest(http.MethodGet, "/agent/daily/activity?start_date="+day+"&end_date="+day, nil)
	req.Header.Set("Authorization", "Bearer scoped")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"total_tokens":4`) || strings.Contains(response.Body.String(), "private") {
		t.Fatalf("incorrect activity response %d %s", response.Code, response.Body)
	}
}
