package proxyconfig

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestVertexGeminiUsesProjectEndpointAndBearer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/demo-project/locations/global/publishers/google/models/gemini-2.5-flash:generateContent" ||
			r.Header.Get("Authorization") != "Bearer fixture-oauth" || r.Header.Get("x-goog-api-key") != "" {
			t.Errorf("unexpected Vertex request: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"vertex fixture"}],"role":"model"},"finishReason":"STOP"}]}`)
	}))
	defer upstream.Close()
	config, err := Load(strings.NewReader(fmt.Sprintf(`
model_list:
  - model_name: demo
    litellm_params:
      model: vertex_ai/gemini-2.5-flash
      api_base: %q
      api_key: fixture-oauth
      vertex_project: demo-project
      vertex_location: global
`, upstream.URL)))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Routes: config.Routes})
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"demo","messages":[]}`))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "vertex fixture") {
		t.Fatalf("Vertex response: %d %s", response.Code, response.Body)
	}
}
