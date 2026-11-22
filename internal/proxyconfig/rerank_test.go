package proxyconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestCohereAndJinaRerank(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if r.URL.Path == "/v2/rerank" && request["model"] != "cohere-fixture" {
			t.Error("wrong Cohere model ID")
		} else if r.URL.Path == "/v1/rerank" && (request["model"] != "jina-fixture" || request["rank_fields"] != nil) {
			t.Error("wrong Jina request")
		} else if r.URL.Path != "/v1/rerank" && r.URL.Path != "/v2/rerank" {
			t.Errorf("unexpected rerank path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"index":0,"relevance_score":0.9}]}`))
	}))
	defer upstream.Close()
	t.Setenv("TEST_RERANK_BASE", upstream.URL)
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: cohere
    litellm_params: {model: cohere/cohere-fixture, api_base: os.environ/TEST_RERANK_BASE}
  - model_name: jina
    litellm_params: {model: jina_ai/jina-fixture, api_base: os.environ/TEST_RERANK_BASE}
`))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Routes: config.Routes})
	for _, model := range []string{"cohere", "jina"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(`{"model":"`+model+`","query":"fixture","documents":["document"],"rank_fields":["text"]}`))
		result := httptest.NewRecorder()
		server.ServeHTTP(result, req)
		if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"relevance_score":0.9`) {
			t.Fatalf("unexpected rerank result: %d %s", result.Code, result.Body.String())
		}
	}
}
