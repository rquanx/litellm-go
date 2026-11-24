package proxyconfig

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestBudgetedKeyPreservesSpendAfterRotation(t *testing.T) {
	dsn := os.Getenv("MINATO_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("MINATO_TEST_POSTGRES_URL is unset")
	}
	db, err := OpenPostgresState(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spend, err := OpenSpendLog(ctx, db, keys)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"fixture","object":"chat.completion","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`)
	}))
	defer upstream.Close()
	config, err := Load(strings.NewReader(`model_list:
  - model_name: demo
    litellm_params:
      model: openai/gpt-4o-mini
      api_base: ` + upstream.URL + `
      input_cost_per_token: 0.25
      output_cost_per_token: 0
`))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := WithBudgetManagement(keys.Handler(runtime.NewServer(runtime.Config{
		Routes: config.Routes, Token: "master", AuthorizeInference: true, AuthorizeKey: keys.Authorize,
		CheckKeyBudget: spend.CheckKeyBudget, Callback: spend.Start(ctx),
	})), db, keys, "")
	if err != nil {
		t.Fatal(err)
	}
	call := func(path, token, payload string, status int) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+token)
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, req)
		if result.Code != status {
			t.Fatalf("%s returned %d, expected %d: %s", path, result.Code, status, result.Body)
		}
		return result.Body.String()
	}
	var key struct{ Key string }
	if err := json.Unmarshal([]byte(call("/key/generate", "master", `{"max_budget":0.25}`, 200)), &key); err != nil || key.Key == "" {
		t.Fatalf("could not issue budgeted key: %v", err)
	}
	const inference = `{"model":"demo","messages":[]}`
	call("/v1/chat/completions", key.Key, inference, 200)
	body := call("/v1/chat/completions", key.Key, inference, 429)
	if !strings.Contains(body, `"type":"budget_exceeded"`) {
		t.Fatalf("unexpected budget response: %s", body)
	}
	rotated := newKeyToken()
	call("/key/regenerate", "master", `{"key":"`+key.Key+`","new_key":"`+rotated+`","grace_period":"1h"}`, 200)
	call("/v1/chat/completions", rotated, inference, 429)
	call("/v1/chat/completions", key.Key, inference, 429)
	if calls.Load() != 1 {
		t.Fatalf("budgeted requests reached upstream: %d", calls.Load())
	}
	call("/key/update", "master", `{"key":"`+rotated+`","max_budget":null}`, 200)
	call("/v1/chat/completions", rotated, inference, 200)

	var periodic struct{ Key string }
	if err := json.Unmarshal([]byte(call("/key/generate", "master",
		`{"max_budget":0.25,"budget_duration":"1d"}`, 200)), &periodic); err != nil || periodic.Key == "" {
		t.Fatalf("could not issue periodic key: %v", err)
	}
	digest := keyDigest(periodic.Key)
	now := time.Now().UTC()
	oldReset := time.Date(now.Year(), now.Month(), now.Day()-1, 0, 0, 0, 0, time.UTC)
	keys.mu.Lock()
	entry := keys.state.Keys[digest]
	if entry.BudgetResetAt == nil || !entry.BudgetResetAt.After(now) {
		keys.mu.Unlock()
		t.Fatal("expected the first reset to be scheduled")
	}
	entry.BudgetResetAt = &oldReset
	keys.state.Keys[digest] = entry
	keys.mu.Unlock()
	historicalCost := 5.0
	previous := float64(oldReset.Add(-time.Hour).Unix())
	spend.Record(runtime.InferenceEvent{ID: newEntityID(), Model: "demo", KeyDigest: digest,
		StartTime: previous, EndTime: previous + 1, Spend: &historicalCost})
	call("/v1/chat/completions", periodic.Key, inference, 200)
	call("/v1/chat/completions", periodic.Key, inference, 429)

	var multiple struct{ Key string }
	if err := json.Unmarshal([]byte(call("/key/generate", "master",
		`{"max_budget":2,"budget_limits":[{"budget_duration":"1d","max_budget":0.25},{"budget_duration":"7d","max_budget":1}]}`, 200)),
		&multiple); err != nil || multiple.Key == "" {
		t.Fatalf("could not issue key with multiple windows: %v", err)
	}
	call("/v1/chat/completions", multiple.Key, inference, 200)
	body = call("/v1/chat/completions", multiple.Key, inference, 429)
	if !strings.Contains(body, "Key over 1d budget") {
		t.Fatalf("expected the daily window to block inference: %s", body)
	}
	call("/key/update", "master", `{"key":"`+multiple.Key+`","budget_limits":[{"budget_duration":"7d","max_budget":0.25}]}`, 200)
	body = call("/v1/chat/completions", multiple.Key, inference, 429)
	if !strings.Contains(body, "Key over 7d budget") {
		t.Fatalf("expected the existing weekly window to retain spend: %s", body)
	}
	var personal struct{ Key string }
	userID := newEntityID()
	userPayload := `{"user_id":"` + userID + `","max_budget":0.25,"budget_duration":"1d"}`
	if err := json.Unmarshal([]byte(call("/user/new", "master", userPayload, 200)), &personal); err != nil || personal.Key == "" {
		t.Fatalf("could not issue personal key: %v", err)
	}
	call("/v1/chat/completions", personal.Key, inference, 200)
	body = call("/v1/chat/completions", personal.Key, inference, 429)
	if !strings.Contains(body, "ExceededBudget: User=") {
		t.Fatalf("expected the user cap to block inference: %s", body)
	}
	teamID := newEntityID()
	call("/team/new", "master", `{"team_id":"`+teamID+`","max_budget":0.25,"budget_duration":"1d"}`, 200)
	var teamKeys [2]struct{ Key string }
	for i := range teamKeys {
		if err := json.Unmarshal([]byte(call("/key/generate", "master", `{"team_id":"`+teamID+`"}`, 200)), &teamKeys[i]); err != nil || teamKeys[i].Key == "" {
			t.Fatalf("could not issue team key: %v", err)
		}
		call("/v1/chat/completions", teamKeys[i].Key, inference, 200)
	}
	body = call("/v1/chat/completions", teamKeys[0].Key, inference, 429)
	if !strings.Contains(body, "Team="+teamID) {
		t.Fatalf("expected both keys to share a budget: %s", body)
	}
	call("/team/update", "master", `{"team_id":"`+teamID+`","max_budget":null,
		"budget_limits":[{"budget_duration":"1d","max_budget":0.25},{"budget_duration":"7d","max_budget":1}]}`, 200)
	call("/v1/chat/completions", teamKeys[1].Key, inference, 200)
	body = call("/v1/chat/completions", teamKeys[0].Key, inference, 429)
	if !strings.Contains(body, "Team="+teamID+" over 1d budget") {
		t.Fatalf("expected the shared daily window to block inference: %s", body)
	}
	call("/team/update", "master", `{"team_id":"`+teamID+`","budget_limits":[],"team_member_budget":0.5,"team_member_budget_duration":"1d"}`, 200)
	call("/team/member_add", "master", `{"team_id":"`+teamID+`","member":{"user_id":"`+userID+`","role":"user"},"max_budget_in_team":0.25}`, 200)
	var memberKey struct{ Key string }
	if err := json.Unmarshal([]byte(call("/key/generate", "master",
		`{"team_id":"`+teamID+`","user_id":"`+userID+`"}`, 200)), &memberKey); err != nil || memberKey.Key == "" {
		t.Fatalf("could not issue member key: %v", err)
	}
	call("/v1/chat/completions", memberKey.Key, inference, 200)
	body = call("/v1/chat/completions", memberKey.Key, inference, 429)
	if !strings.Contains(body, "ExceededBudget: Team member "+userID) {
		t.Fatalf("expected the member budget to block inference: %s", body)
	}
	call("/v1/chat/completions", teamKeys[0].Key, inference, 200)
	call("/team/member_update", "master", `{"team_id":"`+teamID+`","user_id":"`+userID+`","max_budget_in_team":null}`, 200)
	call("/v1/chat/completions", memberKey.Key, inference, 200)
	call("/v1/chat/completions", memberKey.Key, inference, 429)
	tierID, projectID := newEntityID(), newEntityID()
	call("/budget/new", "master", `{"budget_id":"`+tierID+`","max_budget":0.1}`, 200)
	call("/project/new", "master", `{"project_id":"`+projectID+`","budget_id":"`+tierID+`"}`, 200)
	var projectKey struct{ Key string }
	if err := json.Unmarshal([]byte(call("/key/generate", "master",
		`{"project_id":"`+projectID+`"}`, 200)), &projectKey); err != nil || projectKey.Key == "" {
		t.Fatalf("could not issue project key: %v", err)
	}
	call("/v1/chat/completions", projectKey.Key, inference, 200)
	body = call("/v1/chat/completions", projectKey.Key, inference, 429)
	if !strings.Contains(body, "Project="+projectID) {
		t.Fatalf("expected project budget to block inference: %s", body)
	}
	call("/budget/update", "master", `{"budget_id":"`+tierID+`","max_budget":1}`, 200)
	call("/v1/chat/completions", projectKey.Key, inference, 200)
	snapshot, err := spend.CheckKeyBudget(ctx, projectKey.Key)
	if err != nil || snapshot.Attribution == nil {
		t.Fatalf("could not capture project attribution: %v", err)
	}
	call("/key/update", "master", `{"key":"`+projectKey.Key+`","project_id":""}`, 200)
	charge := 0.75
	nowSeconds := float64(time.Now().Unix())
	spend.Record(runtime.InferenceEvent{ID: newEntityID(), Model: "demo",
		KeyDigest: keyDigest(projectKey.Key), SpendAttribution: snapshot.Attribution,
		StartTime: nowSeconds, EndTime: nowSeconds + 1, Spend: &charge})
	call("/key/update", "master", `{"key":"`+projectKey.Key+`","project_id":"`+projectID+`"}`, 200)
	call("/v1/chat/completions", projectKey.Key, inference, 429)
	orgID, orgTierID, orgTeamID := newEntityID(), newEntityID(), newEntityID()
	call("/budget/new", "master", `{"budget_id":"`+orgTierID+`","max_budget":0.5,"budget_duration":"1d"}`, 200)
	call("/organization/new", "master", `{"organization_id":"`+orgID+`","organization_alias":"fixture","budget_id":"`+orgTierID+`"}`, 200)
	call("/team/new", "master", `{"team_id":"`+orgTeamID+`","organization_id":"`+orgID+`"}`, 200)
	var orgKeys [2]struct{ Key string }
	for i, fields := range []string{`{"organization_id":"`+orgID+`"}`, `{"team_id":"`+orgTeamID+`"}`} {
		if err := json.Unmarshal([]byte(call("/key/generate", "master", fields, 200)), &orgKeys[i]); err != nil || orgKeys[i].Key == "" {
			t.Fatalf("could not issue organization-scoped key: %v", err)
		}
		call("/v1/chat/completions", orgKeys[i].Key, inference, 200)
	}
	body = call("/v1/chat/completions", orgKeys[1].Key, inference, 429)
	if !strings.Contains(body, "Organization="+orgID) {
		t.Fatalf("expected both keys to share the organization cap: %s", body)
	}
	customers, err := OpenCustomers(ctx, db, keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE `+budgetTable+` SET budget_reset_at = CURRENT_TIMESTAMP - INTERVAL '1 minute' WHERE budget_id=$1`, orgTierID); err != nil {
		t.Fatal(err)
	}
	if err := customers.resetExpiredBudgets(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	call("/v1/chat/completions", orgKeys[1].Key, inference, 200)
	keyTierID := newEntityID()
	call("/budget/new", "master", `{"budget_id":"`+keyTierID+`","max_budget":0.25,"budget_duration":"1d"}`, 200)
	var tierKey struct{ Key string }
	if err := json.Unmarshal([]byte(call("/key/generate", "master",
		`{"budget_id":"`+keyTierID+`"}`, 200)), &tierKey); err != nil || tierKey.Key == "" {
		t.Fatalf("could not issue a key with a linked budget: %v", err)
	}
	call("/v1/chat/completions", tierKey.Key, inference, 200)
	call("/v1/chat/completions", tierKey.Key, inference, 429)
	if _, err := db.ExecContext(ctx, `UPDATE `+budgetTable+` SET budget_reset_at = CURRENT_TIMESTAMP - INTERVAL '1 minute' WHERE budget_id=$1`, keyTierID); err != nil {
		t.Fatal(err)
	}
	if err := customers.resetExpiredBudgets(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	call("/v1/chat/completions", tierKey.Key, inference, 200)
	var rateKey struct{ Key string }
	if err := json.Unmarshal([]byte(call("/key/generate", "master", `{"rpm_limit":1}`, 200)), &rateKey); err != nil || rateKey.Key == "" {
		t.Fatalf("could not issue rate-limited key: %v", err)
	}
	call("/v1/chat/completions", rateKey.Key, inference, 200)
	newRateKey := newKeyToken()
	call("/key/regenerate", "master", `{"key":"`+rateKey.Key+`","new_key":"`+newRateKey+`"}`, 200)
	body = call("/v1/chat/completions", newRateKey, inference, 429)
	if !strings.Contains(body, "rate_limit_exceeded") {
		t.Fatalf("expected key rotation to preserve the request window: %s", body)
	}
	call("/key/update", "master", `{"key":"`+newRateKey+`","rpm_limit":null}`, 200)
	call("/v1/chat/completions", newRateKey, inference, 200)
	var tokenRateKey struct{ Key string }
	if err := json.Unmarshal([]byte(call("/key/generate", "master", `{"tpm_limit":1}`, 200)), &tokenRateKey); err != nil || tokenRateKey.Key == "" {
		t.Fatalf("could not issue token-limited key: %v", err)
	}
	call("/v1/chat/completions", tokenRateKey.Key, inference, 200)
	body = call("/v1/chat/completions", tokenRateKey.Key, inference, 429)
	if !strings.Contains(body, "Key token limit exceeded") {
		t.Fatalf("expected the token limit to block a second request: %s", body)
	}
	call("/team/update", "master", `{"team_id":"`+teamID+`","rpm_limit":1}`, 200)
	call("/v1/chat/completions", teamKeys[0].Key, inference, 200)
	call("/v1/chat/completions", teamKeys[1].Key, inference, 429)
	call("/team/update", "master", `{"team_id":"`+teamID+`","rpm_limit":null,"tpm_limit":1}`, 200)
	call("/v1/chat/completions", teamKeys[0].Key, inference, 200)
	call("/v1/chat/completions", teamKeys[1].Key, inference, 429)
	global := Config{GlobalMaxBudget: 0.1, GlobalBudgetDuration: "1d"}
	if err := spend.ConfigureGlobalBudget(ctx, global); err != nil {
		t.Fatal(err)
	}
	anchor := spend.globalBudgetAnchor
	if err := spend.ConfigureGlobalBudget(ctx, global); err != nil || !spend.globalBudgetAnchor.Equal(anchor) {
		t.Fatalf("reopening proxy budget moved its reset: %s, %v", spend.globalBudgetAnchor, err)
	}
	limited := runtime.NewServer(runtime.Config{Token: "master", RequireInferenceAuth: true,
		Routes: config.Routes, CheckGlobalBudget: spend.CheckGlobalBudget})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(inference))
	request.Header.Set("Authorization", "Bearer master")
	result := httptest.NewRecorder()
	limited.ServeHTTP(result, request)
	if result.Code != http.StatusTooManyRequests || !strings.Contains(result.Body.String(), "budget_exceeded") {
		t.Fatalf("expected global budget to block master inference: %d %s", result.Code, result.Body)
	}
}

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
	call("sk-test", "/global/spend", 401, "master key")
	call("master", "/global/spend", 200, `"max_budget":0,"spend":0.01`)
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
	if _, err := db.Exec(`ALTER TABLE "LiteLLM_SpendLogs" ADD COLUMN cache_hit TEXT DEFAULT 'True', ADD COLUMN agent_id TEXT DEFAULT 'historical-agent',
		ADD COLUMN api_base TEXT DEFAULT 'https://example.invalid/v1?credential=fixture',
		ADD COLUMN "completionStartTime" TIMESTAMP DEFAULT '2026-01-01 12:00:00.25',
		ADD COLUMN request_tags JSONB DEFAULT '"[\"legacy\",\"legacy\"]"'::jsonb`); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []int64{1, 0} {
		if count, err := ImportLiteLLMSpend(db); err != nil || count != expected {
			t.Fatalf("imported %d, want %d: %v", count, expected, err)
		}
	}
	var tags string
	if err := db.QueryRow(`SELECT tags::text FROM minato_proxy_spend WHERE request_id = 'historical'`).Scan(&tags); err != nil || tags != "{legacy}" {
		t.Fatalf("could not restore historical tags: %q %v", tags, err)
	}
	var agentID string
	if err := db.QueryRow(`SELECT agent_id FROM minato_proxy_spend WHERE request_id = 'historical'`).Scan(&agentID); err != nil || agentID != "historical-agent" {
		t.Fatalf("could not restore historical agent attribution: %q %v", agentID, err)
	}
	var origin string
	var firstOutput float64
	if err := db.QueryRow(`SELECT api_origin,first_output_seconds FROM minato_proxy_spend WHERE request_id = 'historical'`).
		Scan(&origin, &firstOutput); err != nil || origin != "https://example.invalid" || firstOutput != 0.25 {
		t.Fatalf("unexpected historical streaming metrics: %q %g %v", origin, firstOutput, err)
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
	providers := "/global/spend/provider?start_date=2026-01-01&end_date=2026-01-01"
	if _, err := db.Exec(`INSERT INTO minato_proxy_spend
		(request_id,started_at,ended_at,user_id,provider,model_id,spend,prompt_tokens,completion_tokens,total_tokens)
		VALUES ('other-provider','2026-01-01 23:59:59Z','2026-01-01 23:59:59Z','other-user','anthropic','other',0.02,0,0,0),
		       ('outside-window','2026-01-02 00:00:00Z','2026-01-02 00:00:00Z','fixture-user','vertex_ai','other',0.04,0,0,0)`); err != nil {
		t.Fatal(err)
	}
	call("sk-test", providers, 403, "user-bound key")
	call("master", "/global/spend/provider", 400, "start_date")
	call("master", providers, 200, `[{"provider":"anthropic","spend":0.02},{"provider":"openai","spend":0.015}]`)
	call("member", providers, 200, `[{"provider":"openai","spend":0.015}]`)
	call("member", "/spend/logs/v2?request_id=other-provider", 200, `"total":0`)
	call("member", "/spend/logs/v2?team_id=fixture-team", 403, "Not authorized")
	call("member", "/spend/logs/v2?user_id=other-user", 200, `"total":0`)
	summary := call("member", "/spend/logs?start_date=2026-01-01&end_date=2026-01-01", 200, `"total_requests":1`)
	if !strings.Contains(summary, `"startTime":"2026-01-01"`) || !strings.Contains(summary, `"users":{"fixture-user":0.015}`) ||
		!strings.Contains(summary, `"models":{"fixture-model":0.015}`) {
		t.Fatalf("unexpected legacy spend summary: %s", summary)
	}
	if _, err := db.Exec("UPDATE minato_proxy_spend SET team_id='fixture-team', api_key='', key_alias='', model='fixture', model_group='fixture-group', call_type='completion', status='success' WHERE request_id='other-provider'"); err != nil {
		t.Fatal(err)
	}
	permissions := []string{"/spend/logs"}
	owner.MemberPermissions = &permissions
	keys.state.Teams[owner.ID] = owner
	ui := call("member", "/spend/logs/ui?start_date=2026-01-01+00:00:00&end_date=2026-01-02+01:00:00&team_id=fixture-team&page_size=1&sort_by=spend", 200, `"total":2`)
	if !strings.Contains(ui, "other-provider") || strings.Contains(ui, "historical") || strings.Contains(ui, "outside-window") {
		t.Fatalf("unexpected team spend page: %s", ui)
	}
	call("member", "/spend/logs/v2?team_id=another-team", 403, "Not authorized")
	call("member", "/spend/logs/v2?request_id=other-provider", 200, `"total":1`)
	call("member", "/spend/logs?request_id=other-provider", 200, "[]")
	call("member", "/spend/logs?start_date=2026-01-01&end_date=2026-01-01", 200, `"total_requests":1`)
	if _, err := db.Exec("UPDATE minato_proxy_spend SET team_id='' WHERE request_id='other-provider'"); err != nil {
		t.Fatal(err)
	}
	dayQuery := "?start_date=2026-01-01&end_date=2026-01-01"
	call("sk-test", "/global/activity"+dayQuery, 403, "user-bound key")
	call("master", "/global/activity"+dayQuery, 200, `"sum_api_requests":2,"sum_total_tokens":5`)
	call("member", "/global/activity"+dayQuery, 200, `"date":"Jan 01","api_requests":1,"total_tokens":5`)
	call("member", "/global/activity/model"+dayQuery, 200,
		`[{"model":"fixture-group","daily_data":[{"date":"Jan 01","api_requests":1,"total_tokens":5,"model_group":"fixture-group"}],"sum_api_requests":1,"sum_total_tokens":5}]`)
	call("member", "/v2/user/info", 200, `"teams":["fixture-team"]`)
	call("member", "/v2/team/list?search=FIXTURE&page_size=1", 200, `"total":1`)
	call("member", "/v2/team/list?user_id=other-user", 403, "outside the key's user scope")
	call("member", "/v2/user/info?user_id=other-user", 404, "User not found")
	models := call("member", "/global/spend/models", 200, `"total_tokens":5`)
	if strings.Contains(models, `"total_spend":0.02`) {
		t.Fatalf("another user's spend appeared in scoped model totals: %s", models)
	}
	request := httptest.NewRequest("POST", "/global/spend/end_users", strings.NewReader(
		`{"startTime":"2026-01-01T00:00:00Z","endTime":"2026-01-02T00:00:00Z"}`))
	request.Header.Set("Authorization", "Bearer master")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"total_count":2`) {
		t.Fatalf("unexpected end-user report: %d %s", response.Code, response.Body)
	}
	started := float64(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC).Unix())
	spend.Record(runtime.InferenceEvent{ID: "stream-one", KeyDigest: keyDigest("member"), Model: "fixture-model", ModelGroup: "fixture-group",
		Stream: true, StartTime: started, EndTime: started + 1, FirstOutput: started + 0.25, Completion: 2,
		Status: "success", APISource: "https://provider.invalid"})
	spend.Record(runtime.InferenceEvent{ID: "stream-private", Model: "private-model", ModelGroup: "fixture-group",
		Stream: true, StartTime: started, EndTime: started + 1, FirstOutput: started + 0.5, Status: "success", APISource: "https://private.invalid"})
	metrics := "/model/streaming_metrics?_selected_model_group=fixture-group&startTime=2026-01-01&endTime="
	body := call("member", metrics+"2026-01-01", 200, `"date":"stream-one","https://provider.invalid":0.25`)
	if strings.Contains(body, "private.invalid") {
		t.Fatalf("metrics included another user's provider: %s", body)
	}
	call("master", metrics+"2026-01-02", 200, `"date":"2026-01-01","https://private.invalid":0.5,"https://provider.invalid":0.25`)
	latencyQuery := "?_selected_model_group=fixture-group&startTime=2026-01-01&endTime=2026-01-02"
	call("member", "/model/metrics"+latencyQuery, 200, `"date":"2026-01-01","https://provider.invalid":0.5`)
	t.Setenv("DEFAULT_SLACK_ALERTING_THRESHOLD", "1")
	call("member", "/model/metrics/slow_responses"+latencyQuery, 200, `[{"api_base":"https://provider.invalid","total_count":1,"slow_count":1}]`)
	spend.Record(runtime.InferenceEvent{ID: "rate-limited", KeyDigest: keyDigest("member"), Model: "fixture-model",
		ModelGroup: "fixture-group", StartTime: started, EndTime: started + 1, HTTPStatus: 429, Status: "failure",
		APISource: "https://provider.invalid"})
	call("member", "/model/metrics/exceptions"+latencyQuery, 200,
		`"data":[{"RateLimitError":1,"model":"fixture-model-https://provider.invalid","total_exceptions":1}]`)
	call("member", "/spend/logs/ui/historical", 200, `"messages":null`)
	call("member", "/spend/logs/ui/other-provider", 403, "Not authorized")
	call("member", "/spend/logs/ui/unknown-request", 200, "null")

	event := runtime.InferenceEvent{ID: "payload-disabled", KeyDigest: keyDigest("member"), SessionID: "audit-session",
		StartTime: now, EndTime: now, Model: "demo", Status: "success",
		Messages: json.RawMessage(`[{"role":"user","content":"fixture prompt"}]`),
		Response: json.RawMessage(`{"result":"fixture response"}`)}
	spend.Record(event)
	call("member", "/spend/logs/ui/payload-disabled", 200, `"messages":null`)
	spend.SetStorePayloads(true)
	event.ID = "payload-enabled"
	spend.Record(event)
	detail := call("member", "/spend/logs/ui/payload-enabled", 200, "fixture prompt")
	if !strings.Contains(detail, "fixture response") {
		t.Fatalf("payload response was not recorded: %s", detail)
	}
	page := call("member", "/spend/logs/v2?request_id=payload-enabled", 200, `"total":1`)
	if strings.Contains(page, "fixture prompt") || strings.Contains(page, "fixture response") {
		t.Fatalf("payload leaked into spend list: %s", page)
	}
	if _, err := db.Exec("UPDATE minato_proxy_spend SET session_id='audit-session' WHERE request_id='other-provider'"); err != nil {
		t.Fatal(err)
	}
	call("member", "/spend/logs/session/ui?session_id=audit-session&page_size=1", 200, `"total":2`)
	call("master", "/spend/logs/session/ui?session_id=audit-session", 200, `"total":3`)
	call("member", "/spend/logs/ui?request_id=payload-enabled", 200, `"session_total_count":2`)
	if err := spend.ConfigureProviderBudgets(context.Background(), Config{
		ProviderBudgets: map[string]providerBudget{"openai": {Maximum: 0.01, Duration: "1d"}},
	}); err != nil {
		t.Fatal(err)
	}
	if spend.CheckProviderBudget(context.Background(), "openai") || !spend.CheckProviderBudget(context.Background(), "anthropic") {
		t.Fatal("provider limit did not exclude the overspent provider")
	}
	call("master", "/provider/budgets", 200, `"budget_limit":0.01`)
	call("member", "/provider/budgets", 401, "master key")
	event.ID = "user-agent-fixture"
	event.Tags = []string{"User-Agent: claude-cli"}
	event.Total = 4
	if err := spend.Record(event); err != nil {
		t.Fatal(err)
	}
	call("master", "/tag/distinct", 200, "User-Agent: claude-cli")
	for _, path := range []string{"/tag/dau", "/tag/wau", "/tag/mau"} {
		call("master", path+"?tag_filters=User-Agent%3A+claude-cli", 200, `"active_users":1`)
	}
	call("master", "/tag/user-agent/per-user-analytics?tag_filter=claude-cli", 200, `"total_count":1`)
	call("member", "/in_product_nudges", 200, `"is_claude_code_enabled":true`)
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
