package runtime

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
