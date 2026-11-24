package proxyconfig

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rquanx/litellm-go/internal/runtime"
)

func TestProviderRerank(t *testing.T) {
	var iamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/identity/token" {
			iamCalls.Add(1)
			if err := r.ParseForm(); err != nil || r.Form.Get("apikey") != "fixture-ibm" ||
				r.Form.Get("grant_type") != "urn:ibm:params:oauth:grant-type:apikey" {
				t.Error("unexpected IBM IAM request")
			}
			w.Write([]byte(`{"access_token":"fixture-iam","expires_in":600}`))
			return
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/ml/v1/text/rerank":
			if r.Header.Get("Authorization") != "Bearer fixture-iam" || r.URL.Query().Get("version") != "2024-03-13" ||
				request["model_id"] != "ibm/ranker" || request["project_id"] != "fixture-project" {
				t.Errorf("unexpected watsonx request: %+v", request)
			}
			w.Write([]byte(`{"model_id":"ibm/ranker","results":[{"index":0,"score":0.9,"input":"fixture"}],"input_token_count":3}`))
			return
		case "/hf/rerank":
			texts, _ := request["texts"].([]any)
			if r.Header.Get("Authorization") != "Bearer fixture-hf" || len(texts) != 1 ||
				texts[0] != "document" || request["query"] != "fixture" || request["return_text"] != true ||
				request["raw_scores"] != false || request["truncation_direction"] != "Right" {
				t.Errorf("unexpected Hugging Face request: %+v", request)
			}
			w.Write([]byte(`[{"index":0,"score":0.9}]`))
			return
		case "/providers/cohere/v2/rerank":
			if request["model"] != "azure-ranker" || r.Header.Get("Authorization") != "Bearer fixture-azure-ai" {
				t.Errorf("unexpected Azure AI request: %+v", request)
			}
			w.Write([]byte(`{"id":"azure-job","results":[{"index":0,"relevance_score":0.9}],"meta":{"billed_units":{"search_units":1}}}`))
			return
		case "/v1/retrieval/nvidia/ranker-3_2/reranking", "/v1/ranking":
			query, _ := request["query"].(map[string]any)
			passages, _ := request["passages"].([]any)
			if query["text"] != "fixture" || len(passages) != 1 || request["model"] != "nvidia/ranker-3.2" ||
				request["top_k"] != float64(1) || r.Header.Get("Authorization") != "Bearer fixture-nvidia" {
				t.Errorf("unexpected NVIDIA NIM request: %+v", request)
			}
			w.Write([]byte(`{"id":"nvidia-job","rankings":[{"index":0,"logit":0.9}],"usage":{"total_tokens":5}}`))
			return
		case "/deep/v1/inference/org/ranker":
			queries, _ := request["queries"].([]any)
			if len(queries) != 1 || queries[0] != "fixture" || request["model"] != nil ||
				r.Header.Get("Authorization") != "Bearer fixture-deepinfra" {
				t.Errorf("unexpected DeepInfra request: %+v", request)
			}
			w.Write([]byte(`{"request_id":"deep-job","scores":[0.9],"input_tokens":3}`))
			return
		case "/infinity/rerank":
			if request["model"] != "infinity-ranker" || r.Header.Get("Authorization") != "Bearer fixture-infinity" {
				t.Errorf("unexpected Infinity request: %+v", request)
			}
			w.Write([]byte(`{"id":"infinity-job","results":[{"index":0,"relevance_score":0.9,"document":"fixture"}],"usage":{"total_tokens":4,"prompt_tokens":3}}`))
			return
		case "/together/v1/rerank":
			if request["model"] != "org/together-ranker" || r.Header.Get("Authorization") != "Bearer fixture-together" {
				t.Errorf("unexpected Together AI request: %+v", request)
			}
			w.Write([]byte(`{"id":"together-job","results":[{"index":0,"relevance_score":0.9,"document":{"text":"fixture"}}],"usage":{"total_tokens":4,"input_tokens":3,"output_tokens":1}}`))
			return
		}
		if request["model"] == "fireworks/demo" {
			if r.URL.Path != "/inference/v1/rerank" || r.Header.Get("Authorization") != "Bearer fixture-fireworks" ||
				request["top_n"] != float64(1) || request["rank_fields"] != nil || request["return_documents"] != true {
				t.Errorf("unexpected Fireworks request: %s %+v", r.URL.Path, request)
			}
			w.Write([]byte(`{"model":"fireworks/demo","data":[{"index":0,"relevance_score":0.9,"document":"fixture"}],"usage":{"total_tokens":4,"prompt_tokens":3,"completion_tokens":1}}`))
			return
		}
		if request["model"] == "local-ranker" {
			if r.URL.Path != "/rerank" || r.Header.Get("Authorization") != "Bearer fixture-vllm" ||
				request["rank_fields"] == nil || request["return_documents"] != true {
				t.Errorf("unexpected hosted vLLM request: %s %+v", r.URL.Path, request)
			}
			w.Write([]byte(`{"id":"local-job","results":[{"index":0,"relevance_score":0.9,"document":{"text":"fixture"}}],"usage":{"total_tokens":4}}`))
			return
		}
		if r.URL.Path == "/rerank" {
			if request["rerankingConfiguration"] == nil || request["sources"] == nil ||
				r.Header.Get("Authorization") != "Bearer fixture-bedrock" {
				t.Error("invalid Bedrock rerank request")
			}
			w.Write([]byte(`{"results":[{"index":0,"relevanceScore":0.9}]}`))
			return
		}
		if request["model"] == "semantic-ranker-fixture" {
			if r.URL.Path != "/v1/projects/demo-project/locations/global/rankingConfigs/default_ranking_config:rank" ||
				r.Header.Get("Authorization") != "Bearer fixture-vertex" || request["records"] == nil {
				t.Errorf("unexpected Vertex ranking request: %s %+v", r.URL.Path, request)
			}
			w.Write([]byte(`{"records":[{"id":"0","score":0.9}]}`))
			return
		}
		if request["model"] == "voyage-fixture" {
			if r.URL.Path != "/v1/rerank" || request["top_k"] != float64(1) || request["top_n"] != nil {
				t.Errorf("unexpected Voyage ranking request: %s %+v", r.URL.Path, request)
			}
			w.Write([]byte(`{"data":[{"index":0,"relevance_score":0.9}],"usage":{"total_tokens":3}}`))
			return
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
	t.Setenv("INFINITY_API_BASE", upstream.URL+"/infinity")
	t.Setenv("INFINITY_API_KEY", "fixture-infinity")
	t.Setenv("DEEPINFRA_API_BASE", upstream.URL+"/deep/v1/openai")
	t.Setenv("DEEPINFRA_API_KEY", "fixture-deepinfra")
	t.Setenv("TEST_TOGETHER_RERANK_BASE", upstream.URL+"/together")
	t.Setenv("TOGETHERAI_API_KEY", "fixture-together")
	t.Setenv("NVIDIA_NIM_API_BASE", upstream.URL+"/v1")
	t.Setenv("NVIDIA_NIM_API_KEY", "fixture-nvidia")
	t.Setenv("AZURE_AI_API_BASE", upstream.URL+"/providers/cohere/v2")
	t.Setenv("AZURE_AI_API_KEY", "fixture-azure-ai")
	t.Setenv("HF_API_BASE", upstream.URL+"/hf")
	t.Setenv("HUGGINGFACE_API_KEY", "fixture-hf")
	t.Setenv("WATSONX_IAM_URL", upstream.URL+"/identity/token")
	t.Setenv("WATSONX_TOKEN", "")
	t.Setenv("WATSONX_ZENAPIKEY", "")
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: cohere
    litellm_params: {model: cohere/cohere-fixture, api_base: os.environ/TEST_RERANK_BASE}
  - model_name: jina
    litellm_params: {model: jina_ai/jina-fixture, api_base: os.environ/TEST_RERANK_BASE}
  - model_name: voyage
    litellm_params: {model: voyage/voyage-fixture, api_base: os.environ/TEST_RERANK_BASE}
  - model_name: vertex
    litellm_params: {model: vertex_ai/semantic-ranker-fixture, api_base: os.environ/TEST_RERANK_BASE, api_key: fixture-vertex, vertex_project: demo-project}
  - model_name: bedrock
    litellm_params: {model: bedrock/cohere.rerank-v3-5:0, api_base: os.environ/TEST_RERANK_BASE, api_key: fixture-bedrock, aws_region_name: us-east-1}
  - model_name: fireworks
    litellm_params: {model: fireworks_ai/demo, api_base: os.environ/TEST_RERANK_BASE, api_key: fixture-fireworks}
    model_info: {mode: rerank}
  - model_name: local
    litellm_params: {model: hosted_vllm/local-ranker, api_base: os.environ/TEST_RERANK_BASE, api_key: fixture-vllm}
    model_info: {supported_endpoints: [/v1/rerank]}
  - model_name: infinity
    litellm_params: {model: infinity/infinity-ranker}
    model_info: {mode: rerank}
  - model_name: deepinfra
    litellm_params: {model: deepinfra/org/ranker}
    model_info: {mode: rerank}
  - model_name: together
    litellm_params: {model: together_ai/org/together-ranker, api_base: os.environ/TEST_TOGETHER_RERANK_BASE}
    model_info: {mode: rerank}
  - model_name: nvidia
    litellm_params: {model: nvidia_nim/nvidia/ranker-3_2}
    model_info: {mode: rerank}
  - model_name: nvidia-ranking
    litellm_params: {model: nvidia_nim/ranking/nvidia/ranker-3_2}
    model_info: {mode: rerank}
  - model_name: azure-ai
    litellm_params: {model: azure_ai/azure-ranker}
    model_info: {mode: rerank}
  - model_name: hf
    litellm_params: {model: huggingface/org/hf-ranker}
    model_info: {mode: rerank}
  - model_name: watsonx
    litellm_params: {model: watsonx/ibm/ranker, api_base: os.environ/TEST_RERANK_BASE, api_key: fixture-ibm, watsonx_project: fixture-project}
    model_info: {mode: rerank}
`))
	if err != nil {
		t.Fatal(err)
	}
	server := runtime.NewServer(runtime.Config{Routes: config.Routes})
	for _, model := range []string{"cohere", "jina", "voyage", "vertex", "bedrock", "fireworks", "local", "infinity", "deepinfra", "together", "nvidia", "nvidia-ranking", "azure-ai", "hf", "watsonx", "watsonx"} {
		body := `{"model":"` + model + `","query":"fixture","documents":["document"],"rank_fields":["text"],"top_n":1}`
		if model == "hf" {
			body = strings.TrimSuffix(body, "}") + `,"return_documents":true}`
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(body))
		result := httptest.NewRecorder()
		server.ServeHTTP(result, req)
		if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"relevance_score":0.9`) {
			t.Fatalf("unexpected rerank result: %d %s", result.Code, result.Body.String())
		}
		if model == "hf" && (!strings.Contains(result.Body.String(), `"document":{"text":"document"}`) ||
			!strings.Contains(result.Body.String(), `"search_units":1`)) {
			t.Fatalf("unexpected Hugging Face document or usage: %s", result.Body.String())
		}
		var expected string
		switch model {
		case "fireworks":
			expected = `{"id":"fireworks/demo","meta":{"billed_units":{"search_units":4},"tokens":{"input_tokens":3,"output_tokens":1}},"results":[{"index":0,"relevance_score":0.9,"document":{"text":"fixture"}}]}`
		case "local":
			expected = `{"id":"local-job","meta":{"billed_units":{"total_tokens":4},"tokens":{"input_tokens":4}},"results":[{"index":0,"relevance_score":0.9,"document":{"text":"fixture"}}]}`
		case "infinity":
			expected = `{"id":"infinity-job","meta":{"billed_units":{"total_tokens":4},"tokens":{"input_tokens":3,"output_tokens":1}},"results":[{"document":{"text":"fixture"},"index":0,"relevance_score":0.9}]}`
		case "deepinfra":
			expected = `{"id":"deep-job","meta":{"billed_units":{"total_tokens":3},"tokens":{"input_tokens":3,"output_tokens":0}},"results":[{"index":0,"relevance_score":0.9}]}`
		case "together":
			expected = `{"id":"together-job","meta":{"billed_units":{"total_tokens":4},"tokens":{"input_tokens":3,"output_tokens":1}},"results":[{"document":{"text":"fixture"},"index":0,"relevance_score":0.9}]}`
		case "nvidia", "nvidia-ranking":
			expected = `{"id":"nvidia-job","meta":{"billed_units":{"total_tokens":5}},"results":[{"document":{"text":"document"},"index":0,"relevance_score":0.9}]}`
		case "azure-ai":
			expected = `{"id":"azure-job","results":[{"index":0,"relevance_score":0.9}],"meta":{"billed_units":{"search_units":1}}}`
		case "watsonx":
			expected = `{"id":"ibm/ranker","meta":{"tokens":{"input_tokens":3}},"results":[{"document":{"text":"fixture"},"index":0,"relevance_score":0.9}]}`
		}
		if expected != "" && strings.TrimSpace(result.Body.String()) != expected {
			t.Fatalf("unexpected %s response: %s", model, result.Body.String())
		}
	}
	if calls := iamCalls.Load(); calls != 1 {
		t.Fatalf("IBM IAM called %d times, want 1", calls)
	}
}
