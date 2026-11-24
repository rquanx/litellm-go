package proxyconfig

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestChatParameterPolicyWithFallbackAndCache(t *testing.T) {
	var upstreamCalls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		for _, name := range []string{"drop_params", "allowed_openai_params", "additional_drop_params", "temperature", "max_tokens"} {
			if _, found := payload[name]; found {
				t.Errorf("unexpected provider parameter %s", name)
			}
		}
		if payload["model"] != "o3" || payload["max_completion_tokens"] != float64(5) {
			t.Errorf("unexpected provider payload %v", payload)
		}
		tools := payload["tools"].([]any)
		for _, tool := range tools {
			fn := tool.(map[string]any)["function"].(map[string]any)
			if fn["description"] != nil || fn["name"] == nil {
				t.Errorf("unexpected function fields %v", fn)
			}
		}
		if payload["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"result","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer origin.Close()
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unavailable.Close()

	config, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: demo
    litellm_params: {model: openai_like/fixture, api_base: %s}
  - model_name: fallback
    litellm_params:
      model: openai/o3
      api_base: %s
      additional_drop_params: ['tools[*].function.description']
router_settings:
  fallbacks: [{demo: [fallback]}]
litellm_settings:
  drop_params: true
`, unavailable.URL, origin.URL)))
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range config.Routes {
		if route.Alias == "fallback" && !slices.Contains(route.Candidates[0].SupportedOpenAIParams, "reasoning_effort") {
			t.Fatal("known model capabilities are missing")
		}
	}
	server := runtime.NewServer(runtime.Config{Routes: config.Routes, Token: "master", AuthorizeInference: true,
		DropParams: config.DropParams, InferenceCache: runtime.InferenceCacheConfig{Enabled: true, TTL: time.Minute,
			Endpoints: map[string]bool{"chat-completions": true}}})
	body := `{"model":"demo","messages":[{"role":"user","content":"hello"}],"temperature":0.5,"drop_params":false,"max_tokens":5,
		"tools":[{"type":"function","function":{"name":"run","description":"discard"}}]}`
	call := func(path, body string, code int) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer master")
		res := httptest.NewRecorder()
		server.ServeHTTP(res, req)
		if res.Code != code {
			t.Fatalf("status %d, expected %d: %s", res.Code, code, res.Body)
		}
		return res
	}
	call("/v1/chat/completions", body, http.StatusOK)
	direct := strings.Replace(body, `"demo"`, `"fallback"`, 1)
	call("/v1/chat/completions", direct, http.StatusOK)
	if hit := call("/v1/chat/completions", direct, http.StatusOK).Header().Get("X-LiteLLM-Cache-Key"); hit == "" || upstreamCalls.Load() != 2 {
		t.Fatalf("expected the same transformed request to hit cache, provider calls %d", upstreamCalls.Load())
	}
	stream := strings.Replace(direct, `"model":`, `"stream":true,"model":`, 1)
	if res := call("/v1/chat/completions", stream, http.StatusOK); !strings.Contains(res.Body.String(), "[DONE]") {
		t.Fatalf("stream did not complete: %s", res.Body)
	}
	preview := call("/utils/transform_request", `{"call_type":"completion","request_body":`+direct+`}`, http.StatusOK)
	if strings.Contains(preview.Body.String(), "description") || !strings.Contains(preview.Body.String(), "max_completion_tokens") ||
		upstreamCalls.Load() != 3 {
		t.Fatalf("incorrect transformed request preview: %s", preview.Body)
	}
}
