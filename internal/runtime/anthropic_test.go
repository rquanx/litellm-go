package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIClientsCanFallBackToAnthropic(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "fixture-key" ||
			payload["model"] != "claude-fixture" || payload["max_tokens"] != float64(50) || payload["system"] == nil {
			t.Errorf("unexpected anthropic request: %s %#v", r.URL.Path, payload)
		}
		if !inputBool(payload, "stream") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg_test","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2,"cache_read_input_tokens":3}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"message_start","message":{"id":"msg_test","usage":{"input_tokens":5,"cache_read_input_tokens":3}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`{"type":"message_stop"}`,
		} {
			_, _ = io.WriteString(w, "data: "+event+"\n\n")
		}
	}))
	defer upstream.Close()
	server := NewServer(Config{Routes: []Route{{Alias: "demo", Candidates: []Candidate{
		{ModelID: "primary", APIBase: upstream.URL, Endpoint: "chat-completions"},
		{ModelID: "claude-fixture", APIBase: upstream.URL, APIKey: "fixture-key", Endpoint: "anthropic-messages"},
	}}}})
	for _, test := range []struct {
		path, body, want string
	}{
		{"/v1/chat/completions", `{"model":"demo","max_tokens":50,"messages":[{"role":"system","content":"test"},{"role":"user","content":"hello"}]}`, `"object":"chat.completion"`},
		{"/v1/responses", `{"model":"demo","max_output_tokens":50,"instructions":"test","input":"hello","stream":true}`, `"type":"response.completed"`},
	} {
		req := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
		result := httptest.NewRecorder()
		server.ServeHTTP(result, req)
		if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), test.want) ||
			!strings.Contains(result.Body.String(), `"total_tokens":10`) || result.Header().Get("X-Minato-Runtime-Attempts") != "2" {
			t.Fatalf("%s returned %d %q", test.path, result.Code, result.Body.String())
		}
	}
}
