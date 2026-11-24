package proxyconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestLiteLLMCostEndpointContracts(t *testing.T) {
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: priced
    litellm_params:
      model: openai/gpt-4o-mini
      input_cost_per_token: 0.01
      output_cost_per_token: 0.02
`))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := WithModelManagement(runtime.NewServer(runtime.Config{}), config, "master", "")
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string, status int) map[string]any {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer master")
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, request)
		var response map[string]any
		if result.Code != status || json.Unmarshal(result.Body.Bytes(), &response) != nil {
			t.Fatalf("%s: got %d %s", path, result.Code, result.Body)
		}
		return response
	}
	estimate := call(http.MethodPost, "/cost/estimate",
		`{"model":"priced","input_tokens":2,"output_tokens":4,"num_requests_per_day":10}`, 200)
	if estimate["cost_per_request"] != .1 || estimate["daily_cost"] != 1.0 || estimate["monthly_cost"] != nil {
		t.Fatalf("unexpected cost breakdown: %v", estimate)
	}
	spend := call(http.MethodPost, "/spend/calculate",
		`{"completion_response":{"model":"priced","id":"fixture","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":4,"total_tokens":6}}}`, 200)
	if len(spend) != 1 || spend["cost"] != .1 {
		t.Fatalf("unexpected spend: %v", spend)
	}
	messageSpend := call(http.MethodPost, "/spend/calculate",
		`{"model":"priced","messages":[{"role":"user","content":"hello"}]}`, 200)
	if cost, ok := messageSpend["cost"].(float64); !ok || cost <= 0 {
		t.Fatalf("expected priced chat messages: %v", messageSpend)
	}
	call(http.MethodPost, "/cost/estimate", `{"model":"priced","input_tokens":-1}`, 400)
	source := call(http.MethodGet, "/model/cost_map/source", "", 200)
	if source["source"] != "local" || source["url"] != nil || source["model_count"].(float64) <= 0 {
		t.Fatalf("unexpected source: %v", source)
	}
	type providerSettings struct {
		Name string `json:"name"`
	}
	result := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/model/settings", nil)
	request.Header.Set("Authorization", "Bearer master")
	handler.ServeHTTP(result, request)
	var settings []providerSettings
	if result.Code != http.StatusOK || json.Unmarshal(result.Body.Bytes(), &settings) != nil ||
		!slices.Contains(settings, providerSettings{Name: "openai"}) {
		t.Fatalf("expected provider settings including OpenAI, got status %d and %d entries", result.Code, len(settings))
	}
	result = httptest.NewRecorder()
	handler.ServeHTTP(result, httptest.NewRequest(http.MethodGet, "/model/settings", nil))
	if result.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated model settings: got %d", result.Code)
	}
}
