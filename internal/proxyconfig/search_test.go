package proxyconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestPostgresImportsLiteLLMSearchTools(t *testing.T) {
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
	if _, err := db.Exec(`CREATE TEMP TABLE minato_proxy_state (name TEXT PRIMARY KEY, revision BIGINT NOT NULL, value JSONB NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TEMP TABLE "LiteLLM_SearchToolsTable" (
		search_tool_id TEXT, search_tool_name TEXT, litellm_params JSONB, search_tool_info JSONB,
		created_at TIMESTAMP, updated_at TIMESTAMP
	)`); err != nil {
		t.Fatal(err)
	}
	params := `{"search_provider":"perplexity","api_key":"fixture-imported-search-secret"}`
	if _, err := db.Exec(`INSERT INTO "LiteLLM_SearchToolsTable" VALUES
		('fixture-id', 'imported-search', to_jsonb($1::text), '{"description":"fixture"}'::jsonb, now(), now())`, params); err != nil {
		t.Fatal(err)
	}
	cfg := Config{MasterKey: "fixture-master"}
	if count, err := ImportLiteLLMSearchTools(db, cfg); err != nil || count != 1 {
		t.Fatalf("import count %d: %v", count, err)
	}
	var state modelState
	if err := postgresModels(db, cfg.MasterKey).load(&state); err != nil || len(state.SearchTools) != 1 ||
		state.SearchTools[0].ID != "fixture-id" || state.SearchTools[0].Params.APIKey != "fixture-imported-search-secret" {
		t.Fatalf("search tool state did not round-trip: %v", err)
	}
	if _, err := ImportLiteLLMSearchTools(db, cfg); err == nil {
		t.Fatal("import must not overwrite existing search tools")
	}
}

func TestSearchUsesEffectiveKeyAndTeamPermissions(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/broken/search" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path != "/search" || r.Header.Get("Authorization") != "Bearer fixture-provider" {
			t.Error("unexpected search upstream request")
		}
		w.Write([]byte(`{"results":[{"title":"Example","url":"https://example.test","snippet":"Fixture"}]}`))
	}))
	defer upstream.Close()
	t.Setenv("TEST_SEARCH_BASE", upstream.URL)
	t.Setenv("TEST_SEARCH_BROKEN", upstream.URL+"/broken")
	cfg, err := Load(strings.NewReader(`
search_tools:
  - search_tool_name: shared
    litellm_params: {search_provider: perplexity, api_key: fixture-provider, api_base: os.environ/TEST_SEARCH_BASE}
  - search_tool_name: key-only
    litellm_params: {search_provider: perplexity, api_key: fixture-provider, api_base: os.environ/TEST_SEARCH_BASE}
  - search_tool_name: team-only
    litellm_params: {search_provider: perplexity, api_key: fixture-provider, api_base: os.environ/TEST_SEARCH_BASE}
  - search_tool_name: primary
    litellm_params: {search_provider: perplexity, api_key: fixture-provider, api_base: os.environ/TEST_SEARCH_BROKEN}
router_settings:
  fallbacks: [{primary: [shared]}]
`))
	if err != nil {
		t.Fatal(err)
	}
	const master = "fixture-master"
	registry, err := LoadKeys(master, "")
	if err != nil {
		t.Fatal(err)
	}
	var events []runtime.InferenceEvent
	handler := registry.Handler(runtime.NewServer(runtime.Config{
		Token: master, AuthorizeInference: true, AuthorizeKey: registry.Authorize,
		AuthorizeSearch: registry.AuthorizeSearch, SearchTools: cfg.SearchTools, SearchFallbacks: cfg.SearchFallbacks,
		Callback: func(event runtime.InferenceEvent) { events = append(events, event) },
	}))
	request := func(path, token, body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		handler.ServeHTTP(response, req)
		if response.Code != status {
			t.Fatalf("%s got %d, want %d: %s", path, response.Code, status, response.Body.String())
		}
		return response
	}
	request("/team/new", master, `{"team_id":"fixture-team","object_permission":{"search_tools":["shared","team-only","primary"]}}`, 200)
	key := request("/key/generate", master, `{"team_id":"fixture-team","object_permission":{"search_tools":["shared","key-only","primary"]}}`, 200)
	var issued struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(key.Body.Bytes(), &issued); err != nil || issued.Key == "" {
		t.Fatal("expected virtual key")
	}
	request("/search/shared", issued.Key, `{"search_tool_name":"team-only","query":"fixture"}`, 200)
	request("/search/team-only", issued.Key, `{"query":"fixture"}`, 403)
	request("/v1/search", issued.Key, `{"model":"key-only","query":"fixture"}`, 403)
	request("/v1/search/shared", "wrong-key", `{"query":"fixture"}`, 401)
	request("/v1/search/primary", issued.Key, `{"query":"fixture"}`, 200)
	request("/team/update", master, `{"team_id":"fixture-team","object_permission":{"search_tools":["team-only","primary"]}}`, 200)
	request("/v1/search/shared", issued.Key, `{"query":"fixture"}`, 403)
	request("/v1/search/primary", issued.Key, `{"query":"fixture"}`, 503)
	if calls.Load() != 4 {
		t.Fatalf("upstream received %d calls, want 4", calls.Load())
	}
	if len(events) != 3 || events[0].CallType != "asearch" || events[0].ModelGroup != "shared" ||
		events[0].Provider != "perplexity" || events[0].KeyDigest != keyDigest(issued.Key) || events[0].Total != 0 ||
		events[0].Spend == nil || *events[0].Spend != 0.005 || events[0].Messages != nil || events[0].Response != nil {
		t.Fatalf("unexpected search events: %+v", events)
	}
}

func TestManagedSearchToolSurvivesRestart(t *testing.T) {
	var served atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-search-credential" {
			t.Error("stored search credential missing")
		}
		served.Add(1)
		w.Write([]byte(`{"results":[]}`))
	}))
	defer provider.Close()
	const master = "fixture-master"
	cfg := Config{MasterKey: master}
	state := filepath.Join(t.TempDir(), "models.json")
	open := func() http.Handler {
		t.Helper()
		server := runtime.NewServer(runtime.Config{Token: master, AuthorizeInference: true})
		handler, err := WithModelManagement(server, cfg, master, state)
		if err != nil {
			t.Fatal(err)
		}
		return handler
	}
	handler := open()
	call := func(method, path, token, body string, status int) string {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		output := httptest.NewRecorder()
		handler.ServeHTTP(output, req)
		if output.Code != status || strings.Contains(output.Body.String(), "fixture-search-credential") {
			t.Fatalf("%s %s returned %d, want %d: %s", method, path, output.Code, status, output.Body.String())
		}
		return output.Body.String()
	}
	body := `{"search_tool":{"search_tool_name":"managed","litellm_params":{"search_provider":"perplexity","api_base":"` + provider.URL + `","api_key":"fixture-search-credential"}}}`
	probe := `{"litellm_params":{"search_provider":"perplexity","api_base":"` + provider.URL + `","api_key":"fixture-search-credential"}}`
	call(http.MethodPost, "/search_tools/test_connection", "other", probe, 401)
	probeResult := call(http.MethodPost, "/search_tools/test_connection", master, probe, 200)
	if !strings.Contains(probeResult, `"status":"success"`) {
		t.Fatalf("connection check failed: %s", probeResult)
	}
	call(http.MethodGet, "/search_tools/ui/available_providers", master, "", 200)
	call(http.MethodPost, "/search_tools", "other", body, 401)
	response := call(http.MethodPost, "/search_tools", master, body, 200)
	var created struct {
		ID string `json:"search_tool_id"`
	}
	if json.Unmarshal([]byte(response), &created) != nil || created.ID == "" {
		t.Fatalf("missing search tool ID: %s", response)
	}
	call(http.MethodPost, "/v1/search/managed", master, `{"query":"fixture"}`, 200)
	handler = open()
	path := "/search_tools/" + created.ID
	call(http.MethodGet, path, master, "", 200)
	updated := strings.ReplaceAll(body, "managed", "renamed")
	updated = strings.ReplaceAll(updated, `,"api_key":"fixture-search-credential"`, "")
	call(http.MethodPut, path, master, updated, 200)
	call(http.MethodPost, "/v1/search/managed", master, `{"query":"fixture"}`, 400)
	call(http.MethodPost, "/v1/search/renamed", master, `{"query":"fixture"}`, 200)
	call(http.MethodDelete, path, master, "", 200)
	handler = open()
	call(http.MethodGet, path, master, "", 404)
	if served.Load() != 3 {
		t.Fatalf("unexpected provider call count %d", served.Load())
	}
}
