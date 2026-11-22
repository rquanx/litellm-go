package proxyconfig

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestSpecializedFallbacks(t *testing.T) {
	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct{ Stream bool }
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if input.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"error\":{\"code\":\"content_policy_violation\"}}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":"context_length_exceeded"}}`)
	}))
	defer main.Close()
	var contextCalls, policyCalls atomic.Int32
	context := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contextCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"context fallback"}}]}`)
	}))
	defer context.Close()
	policy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		policyCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"policy fallback\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer policy.Close()
	config, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: primary
    litellm_params: {model: openai/main, api_base: %q}
  - model_name: large
    litellm_params: {model: openai/large, api_base: %q}
  - model_name: alternate
    litellm_params: {model: openai/alternate, api_base: %q}
router_settings:
  context_window_fallbacks:
    - primary: [large]
  content_policy_fallbacks:
    - primary: [alternate]
`, main.URL, context.URL, policy.URL)))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Token: "master", AuthorizeInference: true, Routes: config.Routes,
		AuthorizeKey: func(token string) ([]string, bool) { return []string{"primary"}, token == "restricted" }})
	call := func(token, body string, status int, result string) {
		t.Helper()
		request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != status || !strings.Contains(response.Body.String(), result) {
			t.Fatalf("got %d %s, want %d containing %q", response.Code, response.Body, status, result)
		}
	}
	call("restricted", `{"model":"primary","messages":[]}`, 400, "context_length_exceeded")
	call("master", `{"model":"primary","messages":[]}`, 200, "context fallback")
	call("master", `{"model":"primary","messages":[],"stream":true}`, 200, "policy fallback")
	for token, expect := range map[string]string{"master": `"fallbacks":["large"]`, "restricted": `"fallbacks":[]`} {
		request := httptest.NewRequest("GET", "/models?include_metadata=true&fallback_type=context_window", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != 200 || !strings.Contains(response.Body.String(), expect) {
			t.Fatalf("discovery did not respect the fallback scope: %s", response.Body.String())
		}
	}
	if contextCalls.Load() != 1 || policyCalls.Load() != 1 {
		t.Fatal("a specialized fallback ran for the wrong failure or scope")
	}
}
