package proxyconfig

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestModelInfoV2ScopeAndPagination(t *testing.T) {
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: visible
    litellm_params: {model: openai/gpt-4o-mini, api_key: fixture-secret, input_cost_per_token: 0.01}
    model_info: {id: visible-id, created_at: 2025-01-01T00:00:00Z}
  - model_name: restricted
    litellm_params: {model: openai/gpt-4o-mini}
    model_info: {id: restricted-id}
`))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := LoadKeys("master", "")
	if err != nil {
		t.Fatal(err)
	}
	keys.state.Keys[keyDigest("scoped")] = virtualKey{Models: []string{"visible"}}
	keys.state.Users = map[string]internalUser{"reader": {ID: "reader", Role: "internal_user"}}
	keys.state.Keys[keyDigest("member")] = virtualKey{UserID: "reader"}
	keys.state.Teams["shared"] = team{ID: "shared", Models: []string{"visible"},
		Members: []teamMembership{{UserID: "reader", Role: "user"}}}
	keys.state.Teams["private"] = team{ID: "private", Models: []string{"restricted"}}
	handler, err := WithModelManagement(runtime.NewServer(runtime.Config{}), config, "master", "", keys)
	if err != nil {
		t.Fatal(err)
	}
	call := func(token, query string, status int, expected string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/v2/model/info"+query, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		body := response.Body.String()
		if response.Code != status || !strings.Contains(body, expected) || strings.Contains(body, "fixture-secret") {
			t.Fatalf("%s: %d %s", query, response.Code, body)
		}
	}
	call("master", "?size=1&page=2&sortBy=model_name", 200, `"model_name":"visible"`)
	call("master", "?size=1&sortBy=costs&sortOrder=desc", 200, `"model_name":"visible"`)
	call("master", "?size=1&sortBy=created_at&sortOrder=desc", 200, `"model_name":"visible"`)
	call("master", "?size=1&sortBy=created_at", 200, `"model_name":"visible"`)
	call("master", "?user_models_only=true", 200, `"data":[]`)
	create := httptest.NewRequest(http.MethodPost, "/model/new",
		strings.NewReader(`{"model_name":"managed","litellm_params":{"model":"openai/gpt-4o-mini"}}`))
	create.Header.Set("Authorization", "Bearer master")
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, create)
	if result.Code != http.StatusOK {
		t.Fatalf("could not create managed model: %d %s", result.Code, result.Body)
	}
	call("master", "?user_models_only=true", 200, `"model_name":"managed"`)
	call("master", "?size=1&sortBy=status&sortOrder=desc", 200, `"model_name":"managed"`)
	call("scoped", "?user_models_only=true", 200, `"data":[]`)
	call("member", "?include_team_models=true", 200, `"access_via_team_ids":["shared"]`)
	call("member", "?teamId=shared", 200, `"total_count":1`)
	call("member", "?teamId=private", 200, `"data":[]`)
	call("scoped", "?search=VISIBLE", 200, `"total_count":1`)
	call("scoped", "?modelId=restricted-id", 200, `"data":[]`)
	call("unknown", "", 401, "Proxy token required")
}
