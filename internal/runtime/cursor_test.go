package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCursorChatCompletionsUsesProxyRoutes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		input, _ := payload["input"].([]any)
		if r.URL.Path != "/v1/responses" || payload["model"] != "fixture-upstream" || len(input) != 1 {
			t.Errorf("unexpected upstream request: %s %+v", r.URL.Path, payload)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp-1","object":"response","created_at":1,"output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()
	server := NewServer(Config{
		Token: "master", AuthorizeInference: true, HTTPClient: upstream.Client(),
		Routes: []Route{{Alias: "fixture", Candidates: []Candidate{
			{ModelID: "fixture-upstream", APIBase: upstream.URL + "/v1", Endpoint: "responses"},
		}}},
	})
	for token, expected := range map[string]int{"bad": 401, "master": 200} {
		r := httptest.NewRequest(http.MethodPost, "/cursor/chat/completions",
			strings.NewReader(`{"model":"fixture","input":[{"role":"user","content":"hello"}]}`))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		result := httptest.NewRecorder()
		server.ServeHTTP(result, r)
		if result.Code != expected || (expected == 200 && !strings.Contains(result.Body.String(), `"object":"chat.completion"`)) {
			t.Fatalf("unexpected Cursor result: %d %s", result.Code, result.Body)
		}
	}
}

