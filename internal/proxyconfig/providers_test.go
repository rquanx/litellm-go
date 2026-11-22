package proxyconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestEmbeddedProviderSettingsAffectRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		messages, _ := input["messages"].([]any)
		if r.URL.Path != "/custom/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-key" ||
			input["max_tokens"] != float64(30) || input["max_completion_tokens"] != nil ||
			len(messages) != 1 || messages[0].(map[string]any)["content"] != "hello" {
			t.Errorf("unexpected provider request: %s %#v", r.URL.Path, input)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"fixture","choices":[]}`))
	}))
	defer upstream.Close()
	t.Setenv("PUBLICAI_API_BASE", upstream.URL+"/custom")
	t.Setenv("PUBLICAI_API_KEY", "fixture-key")
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: demo
    litellm_params: {model: publicai/fixture}
`))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Routes: config.Routes})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"demo","max_completion_tokens":30,
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`))
	result := httptest.NewRecorder()
	server.ServeHTTP(result, request)
	if result.Code != http.StatusOK {
		t.Fatalf("provider request failed: %d %s", result.Code, result.Body.String())
	}
}
