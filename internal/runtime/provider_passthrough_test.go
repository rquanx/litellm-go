package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVLLMPassThroughMapsScopedAliases(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer provider-fixture" ||
			json.NewDecoder(r.Body).Decode(&body) != nil || body["model"] != "provider/model" {
			t.Error("vLLM did not receive the resolved deployment and model")
		}
		w.Write([]byte(`{"fixture":true}`))
	}))
	defer origin.Close()
	server := NewServer(Config{Token: "fixture-master", AuthorizeInference: true,
		AuthorizeKey: func(token string) ([]string, bool) { return []string{"allowed"}, token == "fixture-limited" },
		Routes: []Route{{Alias: "allowed", Candidates: []Candidate{{DeploymentID: "id", RoutingGroup: "allowed",
			Provider: "hosted_vllm", APIBase: origin.URL + "/v1", APIKey: "provider-fixture", ModelID: "provider/model"}}}},
	})
	for _, tc := range []struct {
		path, model string
		status int
	}{
		{"/vllm/v1/chat/completions", "allowed", 200},
		{"/vllm/v1/chat/completions", "denied", 403},
		{"/vllm/admin/control", "allowed", 403},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{"model":"`+tc.model+`"}`))
		req.Header.Set("Authorization", "Bearer fixture-limited")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("%s returned %d, want %d", tc.path, w.Code, tc.status)
		}
	}
}

func TestBedrockPassThroughSignsTheResolvedProviderRequest(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/model/application-inference-profile%2Ffixture/converse" ||
			!strings.Contains(r.Header.Get("Authorization"), "Credential=fixture-aws/") ||
			r.Header.Get("X-Amz-Security-Token") != "fixture-session" {
			t.Errorf("incorrect Bedrock target or signature on %s", r.URL.EscapedPath())
		}
		w.Write([]byte(`{"fixture":true}`))
	}))
	defer origin.Close()
	server := NewServer(Config{Token: "fixture-master", AuthorizeInference: true,
		Routes: []Route{{Alias: "fixture", Candidates: []Candidate{{DeploymentID: "id", Provider: "bedrock",
			APIBase: origin.URL, ModelID: "application-inference-profile/fixture", Bedrock: &BedrockConfig{
				Region: "us-east-1", AccessKey: "fixture-aws", SecretKey: "fixture-secret", SessionToken: "fixture-session",
			}}}}},
	})
	request := httptest.NewRequest(http.MethodPost, "/bedrock/model/fixture/converse", strings.NewReader("{}"))
	request.Header.Set("Authorization", "Bearer fixture-master")
	output := httptest.NewRecorder()
	server.ServeHTTP(output, request)
	if output.Code != 200 {
		t.Fatalf("Bedrock pass-through returned %d: %s", output.Code, output.Body.String())
	}
}

func TestProviderPassThroughKeepsGatewayCredentialsLocal(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fixture/v1beta/models":
			if r.URL.Query().Get("key") != "provider-fixture" || r.URL.Query().Get("pageSize") != "2" ||
				r.Header.Get("Authorization") != "" {
				t.Error("Gemini query did not use the configured upstream key")
			}
		case "/fixture/v2/models":
			if r.Header.Get("Authorization") != "Bearer provider-fixture" {
				t.Error("Cohere did not receive its provider credential")
			}
		case "/fixture/v1/threads":
			if r.Header.Get("Authorization") != "Bearer provider-fixture" || r.Header.Get("OpenAI-Beta") != "assistants=v2" {
				t.Error("OpenAI did not receive its provider credential and beta version")
			}
		case "/fixture/v1/models":
			if r.Header.Get("X-Api-Key") != "provider-fixture" || r.Header.Get("Anthropic-Version") != "2023-06-01" {
				t.Error("Anthropic did not receive its provider key and API version")
			}
		case "/fixture/openai/deployments/example/models":
			if r.Header.Get("Api-Key") != "provider-fixture" || r.URL.Query().Get("api-version") != "2024-10-21" {
				t.Error("Azure did not receive the configured key and API version")
			}
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
		}
		w.Write([]byte(`{"fixture":true}`))
	}))
	defer origin.Close()
	t.Setenv("GEMINI_API_BASE", origin.URL+"/fixture")
	t.Setenv("GEMINI_API_KEY", "provider-fixture")
	t.Setenv("COHERE_API_BASE", origin.URL+"/fixture")
	t.Setenv("COHERE_API_KEY", "provider-fixture")
	t.Setenv("OPENAI_API_BASE", origin.URL+"/fixture")
	t.Setenv("OPENAI_API_KEY", "provider-fixture")
	t.Setenv("ANTHROPIC_API_BASE", origin.URL+"/fixture")
	t.Setenv("ANTHROPIC_API_KEY", "provider-fixture")
	t.Setenv("AZURE_API_BASE", origin.URL+"/fixture")
	t.Setenv("AZURE_API_KEY", "provider-fixture")
	server := NewServer(Config{Token: "fixture-master", AuthorizeInference: true,
		AuthorizeKey: func(token string) ([]string, bool) { return []string{"limited"}, token == "fixture-limited" }})
	for _, tc := range []struct {
		path, token string
		status int
	}{
		{"/gemini/v1beta/models?key=fixture-master&pageSize=2", "", 200},
		{"/gemini/v1beta/models?key=wrong", "", 401},
		{"/cohere/v2/models", "fixture-master", 200},
		{"/cohere/v2/models", "fixture-limited", 403},
		{"/openai/v1/threads", "fixture-master", 200},
		{"/openai_passthrough/v1/threads", "fixture-master", 200},
		{"/anthropic/v1/models", "fixture-master", 200},
		{"/azure/openai/deployments/example/models?api-version=2024-10-21", "fixture-master", 200},
		{"/azure_ai/openai/deployments/example/models?api-version=2024-10-21", "fixture-master", 200},
	} {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		if tc.token != "" {
			r.Header.Set("Authorization", "Bearer "+tc.token)
		}
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s returned %d, want %d", tc.path, w.Code, tc.status)
		}
	}
	server.ReplaceConfiguredRouting([]Route{{Alias: "fixture", Candidates: []Candidate{
		{Provider: "cohere", DeploymentID: "fixture-cohere", RoutingGroup: "fixture", APIBase: origin.URL + "/fixture",
			APIKey: "provider-fixture", UseInPassThrough: true},
	}}}, nil, nil)
	t.Setenv("COHERE_API_KEY", "fallback-key")
	t.Setenv("COHERE_API_BASE", "http://127.0.0.1:1")
	r := httptest.NewRequest(http.MethodGet, "/cohere/v2/models", nil)
	r.Header.Set("Authorization", "Bearer fixture-master")
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("configured deployment did not take precedence: %d", w.Code)
	}
}
