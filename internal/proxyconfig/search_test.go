package proxyconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestSearchUsesEffectiveKeyAndTeamPermissions(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/search" || r.Header.Get("Authorization") != "Bearer fixture-provider" {
			t.Error("unexpected search upstream request")
		}
		w.Write([]byte(`{"results":[{"title":"Example","url":"https://example.test","snippet":"Fixture"}]}`))
	}))
	defer upstream.Close()
	t.Setenv("TEST_SEARCH_BASE", upstream.URL)
	cfg, err := Load(strings.NewReader(`
search_tools:
  - search_tool_name: shared
    litellm_params: {search_provider: perplexity, api_key: fixture-provider, api_base: os.environ/TEST_SEARCH_BASE}
  - search_tool_name: key-only
    litellm_params: {search_provider: perplexity, api_key: fixture-provider, api_base: os.environ/TEST_SEARCH_BASE}
  - search_tool_name: team-only
    litellm_params: {search_provider: perplexity, api_key: fixture-provider, api_base: os.environ/TEST_SEARCH_BASE}
`))
	if err != nil {
		t.Fatal(err)
	}
	const master = "fixture-master"
	registry, err := LoadKeys(master, "")
	if err != nil {
		t.Fatal(err)
	}
	handler := registry.Handler(runtime.NewServer(runtime.Config{
		Token: master, AuthorizeInference: true, AuthorizeKey: registry.Authorize,
		AuthorizeSearch: registry.AuthorizeSearch, SearchTools: cfg.SearchTools,
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
	request("/team/new", master, `{"team_id":"fixture-team","object_permission":{"search_tools":["shared","team-only"]}}`, 200)
	key := request("/key/generate", master, `{"team_id":"fixture-team","object_permission":{"search_tools":["shared","key-only"]}}`, 200)
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
	request("/team/update", master, `{"team_id":"fixture-team","object_permission":{"search_tools":["team-only"]}}`, 200)
	request("/v1/search/shared", issued.Key, `{"query":"fixture"}`, 403)
	if calls.Load() != 1 {
		t.Fatalf("upstream received %d calls, want 1", calls.Load())
	}
}
