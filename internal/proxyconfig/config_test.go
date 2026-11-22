package proxyconfig

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func TestConfiguredModelModeSetsInferenceEndpoint(t *testing.T) {
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: embeddings
    litellm_params: {model: openai/text-embedding-3-small}
  - model_name: images
    litellm_params: {model: openai/private-image}
    model_info: {mode: image_generation, supported_endpoints: [/v1/images/generations, /v1/images/edits]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if config.Routes[0].Candidates[0].Endpoint != "embeddings" ||
		len(config.Routes[1].Candidates) != 2 ||
		config.Routes[1].Candidates[0].Endpoint != "image-generation" ||
		config.Routes[1].Candidates[1].Endpoint != "image-edit" {
		t.Fatal("model mode did not select its inference endpoint")
	}
}

func TestLoadLiteLLMProxyConfig(t *testing.T) {
	t.Setenv("TEST_LITELLM_KEY", "fixture-master")
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: summary
    litellm_params:
      model: openai/primary-model
      api_base: http://primary.example/v1
      api_key: os.environ/TEST_LITELLM_KEY
    model_info:
      supported_endpoints: [/v1/responses]
  - model_name: reserve
    litellm_params:
      model: openai/chat_completions/backup-model
      api_base: http://backup.example/v1
general_settings:
  master_key: os.environ/TEST_LITELLM_KEY
router_settings:
  num_retries: 0
  timeout: 20
  fallbacks:
    - summary: [reserve]
`))
	if err != nil {
		t.Fatal(err)
	}
	if config.MasterKey != "fixture-master" || config.Timeout != 20*time.Second || len(config.Routes) != 2 {
		t.Fatal("expected routes, a master key and a timeout")
	}
	route := config.Routes[0]
	if route.Alias != "summary" || len(route.Candidates) != 2 {
		t.Fatal("expected one primary and one fallback")
	}
	primary, reserve := route.Candidates[0], route.Candidates[1]
	if primary.Endpoint != "responses" || primary.ModelID != "primary-model" || primary.APIKey != "fixture-master" ||
		reserve.Endpoint != "chat-completions" || reserve.ModelID != "backup-model" {
		t.Fatal("candidate order, protocol, model or credential changed")
	}
}

func TestLoadRejectsUnsupportedConfiguration(t *testing.T) {
	_, err := Load(strings.NewReader("general_settings:\n  database_url: postgresql://example.invalid\n"))
	if err == nil {
		t.Fatal("unsupported fields must fail during startup")
	}
}

func TestConfiguredProxySwitchesToChatFallback(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" {
			t.Errorf("primary path = %s", request.URL.Path)
		}
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" || request.Header.Get("Authorization") != "Bearer fixture-upstream" {
			t.Errorf("unexpected request to fallback: %s", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"chat-1","object":"chat.completion","choices":[{"message":{"content":"ready"}}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
	}))
	defer backup.Close()
	t.Setenv("TEST_PRIMARY_BASE", primary.URL+"/v1")
	t.Setenv("TEST_BACKUP_BASE", backup.URL+"/v1")
	t.Setenv("TEST_BACKUP_TOKEN", "fixture-upstream")
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: summary
    litellm_params: {model: openai/primary, api_base: os.environ/TEST_PRIMARY_BASE}
    model_info: {supported_endpoints: [/v1/responses]}
  - model_name: backup
    litellm_params: {model: openai/chat_completions/backup, api_base: os.environ/TEST_BACKUP_BASE, api_key: os.environ/TEST_BACKUP_TOKEN}
general_settings: {master_key: fixture-master}
router_settings:
  fallbacks:
    - summary: [backup]
`))
	if err != nil {
		t.Fatal(err)
	}
	proxy := runtime.NewServer(runtime.Config{Routes: config.Routes, Token: config.MasterKey, AuthorizeInference: true})
	for _, test := range []struct {
		token string
		code int
	}{
		{code: http.StatusUnauthorized},
		{token: config.MasterKey, code: http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"summary","input":"hello"}`))
		req.Header.Set("Authorization", "Bearer "+test.token)
		result := httptest.NewRecorder()
		proxy.ServeHTTP(result, req)
		if result.Code != test.code {
			t.Fatalf("status = %d, want %d", result.Code, test.code)
		}
		if test.code == http.StatusOK && (result.Header().Get("X-Minato-Runtime-Model") != "backup" ||
			!strings.Contains(result.Body.String(), `"input_tokens":3`)) {
			t.Fatalf("missing fallback result: %s", result.Body.String())
		}
	}
}
