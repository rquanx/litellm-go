package runtime

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBedrockNativeTokenCount(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		expected := `{"input":{"converse":{"messages":[{"content":[{"text":"fixture"}],"role":"user"}],"system":[{"text":"system"}],"toolConfig":{"tools":[{"toolSpec":{"description":"t_1_tool","inputSchema":{"json":{"properties":{},"type":"object"}},"name":"t_1_tool"}}]}}}}`
		if r.URL.Path != "/model/us.anthropic.claude-3-5-sonnet-20241022-v2:0/count-tokens" ||
			r.Header.Get("Authorization") != "Bearer fixture" || string(body) != expected {
			t.Errorf("unexpected Bedrock token request %s: %s", r.URL, body)
		}
		io.WriteString(w, `{"inputTokens":42}`)
	}))
	defer upstream.Close()
	server := NewServer(Config{AuthorizeInference: true, Routes: []Route{{Alias: "demo", Candidates: []Candidate{
		{Provider: "bedrock", ModelID: "us.anthropic.claude-3-5-sonnet-20241022-v2:0", Endpoint: "bedrock-converse",
			APIBase: upstream.URL, APIKey: "fixture", Bedrock: &BedrockConfig{Region: "us-east-1"}},
	}}}})
	payload := `{"model":"demo","messages":[{"role":"user","content":"fixture"}],"system":"system","tools":[{"name":"1-tool"}]}`
	request := httptest.NewRequest("POST", "/utils/token_counter?call_endpoint=true", strings.NewReader(payload))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"tokenizer_type":"bedrock_api"`) ||
		!strings.Contains(response.Body.String(), `"total_tokens":42`) {
		t.Fatalf("unexpected token count %d: %s", response.Code, response.Body)
	}
	response = httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(payload)))
	if response.Code != 200 || strings.TrimSpace(response.Body.String()) != `{"input_tokens":42}` {
		t.Fatalf("unexpected Anthropic-style token count %d: %s", response.Code, response.Body)
	}
}

func TestTokenCounterUsesScopedProviderOrLocalTokenizer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/gemini-2.5-flash:countTokens" || r.Header.Get("x-goog-api-key") != "fixture" {
			t.Errorf("unexpected native token request: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"totalTokens":9}`))
	}))
	defer upstream.Close()
	server := NewServer(Config{Token: "master", AuthorizeInference: true, AuthorizeKey: func(token string) ([]string, bool) {
		return []string{"demo"}, token == "scoped"
	}, Routes: []Route{{Alias: "demo", Candidates: []Candidate{
		{Provider: "gemini", ModelID: "gemini-2.5-flash", Endpoint: "gemini-generate", APIBase: upstream.URL, APIKey: "fixture"},
	}}}})
	call := func(path, token, body string, status int, expected string) {
		t.Helper()
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != status || !strings.Contains(w.Body.String(), expected) {
			t.Fatalf("%s: got %d %s, want %d containing %q", path, w.Code, w.Body, status, expected)
		}
	}
	call("/utils/token_counter", "unknown", `{"model":"demo","prompt":"hello"}`, 401, "unauthorized")
	call("/utils/token_counter", "scoped", `{"model":"gpt-4o","prompt":"hello"}`, 403, "model_not_allowed")
	call("/utils/token_counter", "master", `{"model":"gpt-4o","prompt":"hello world"}`, 200, `"total_tokens":2`)
	call("/utils/token_counter?call_endpoint=true", "scoped",
		`{"model":"demo","contents":[{"parts":[{"text":"fixture"}]}]}`, 200, `"total_tokens":9`)
	call("/v1beta/models/demo:countTokens", "scoped",
		`{"contents":[{"parts":[{"text":"fixture"}]}]}`, 200, `"totalTokens":9`)
}
