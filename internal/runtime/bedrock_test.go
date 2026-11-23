package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

func TestBedrockClaudeAnthropicMessagesInvoke(t *testing.T) {
	const model = "anthropic.claude-3-5-sonnet-20241022-v2:0"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer fixture" || !strings.Contains(string(body), `"anthropic_version":"bedrock-2023-05-31"`) ||
			!strings.Contains(string(body), `"cache_control":{"type":"ephemeral"}`) || strings.Contains(string(body), `"stream":`) ||
			!strings.Contains(string(body), `"anthropic_beta":["context-1m-2025-08-07"]`) {
			t.Errorf("unexpected Bedrock Claude request: %s %s", r.URL.Path, body)
		}
		switch r.URL.Path {
		case "/model/" + model + "/invoke":
			io.WriteString(w, `{"id":"msg_fixture","type":"message","role":"assistant","model":"native","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":3,"output_tokens":1}}`)
		case "/model/" + model + "/invoke-with-response-stream":
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			encoder := eventstream.NewEncoder()
			for _, event := range []string{
				`{"type":"message_start","message":{"id":"msg_fixture","model":"native","usage":{"input_tokens":3,"cache_read_input_tokens":2}}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`{"type":"message_stop","amazon-bedrock-invocationMetrics":{"outputTokenCount":1}}`,
			} {
				payload, _ := json.Marshal(map[string][]byte{"bytes": []byte(event)})
				var headers eventstream.Headers
				headers.Set(":message-type", eventstream.StringValue("event"))
				if err := encoder.Encode(w, eventstream.Message{Headers: headers, Payload: payload}); err != nil {
					t.Error(err)
				}
			}
		default:
			t.Errorf("unexpected Bedrock path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	server := NewServer(Config{Routes: []Route{{Alias: "demo", Candidates: []Candidate{
		{Provider: "bedrock", ModelID: model, APIBase: upstream.URL, Endpoint: "bedrock-converse", APIKey: "fixture",
			Bedrock: &BedrockConfig{Region: "us-east-1"}},
	}}}})
	const payload = `{"model":"demo","max_tokens":20,"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h","scope":"global"}}]}]`
	for _, suffix := range []string{"}", `,"stream":true}`} {
		request := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(payload+suffix))
		request.Header.Set("anthropic-beta", "context-1m-2025-08-07,files-api-2025-04-14")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != 200 || !strings.Contains(response.Body.String(), `"model":"demo"`) ||
			!strings.Contains(response.Body.String(), `"output_tokens":1`) {
			t.Fatalf("unexpected Bedrock Claude response %d %s", response.Code, response.Body)
		}
		if suffix != "}" && !strings.Contains(response.Body.String(), "event: message_stop") {
			t.Fatal("missing Bedrock Claude stream terminal event")
		}
		if suffix != "}" && !strings.Contains(response.Body.String(), `"usage":{"cache_read_input_tokens":2,"output_tokens":1}`) {
			t.Fatal("missing cache usage on the Bedrock Claude message delta")
		}
	}
}

func TestBedrockConverseJSONAndStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(r.Header.Get("Authorization"), "/us-east-1/bedrock/aws4_request") ||
			r.Header.Get("X-Amz-Security-Token") != "fixture-session" ||
			!strings.Contains(string(body), `"maxTokens":20`) || !strings.Contains(string(body), `"text":"hello"`) {
			t.Errorf("unexpected Bedrock request: %s %s", r.URL.Path, body)
		}
		switch r.URL.Path {
		case "/model/fixture/converse":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"ok"}]}},"stopReason":"end_turn","usage":{"inputTokens":3,"outputTokens":1,"totalTokens":4}}`)
		case "/model/fixture/converse-stream":
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			encoder := eventstream.NewEncoder()
			for _, event := range []struct{ kind, payload string }{
				{"messageStart", `{"role":"assistant"}`},
				{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"ok"}}`},
				{"messageStop", `{"stopReason":"end_turn"}`},
				{"metadata", `{"usage":{"inputTokens":3,"outputTokens":1,"totalTokens":4}}`},
			} {
				var headers eventstream.Headers
				headers.Set(":message-type", eventstream.StringValue("event"))
				headers.Set(":event-type", eventstream.StringValue(event.kind))
				if err := encoder.Encode(w, eventstream.Message{Headers: headers, Payload: []byte(event.payload)}); err != nil {
					t.Error(err)
				}
			}
		default:
			t.Errorf("unexpected upstream route %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	server := NewServer(Config{Routes: []Route{{Alias: "demo", Candidates: []Candidate{
		{ModelID: "fixture", APIBase: upstream.URL, Endpoint: "bedrock-converse",
			Bedrock: &BedrockConfig{Region: "us-east-1", AccessKey: "fixture-access", SecretKey: "fixture-secret", SessionToken: "fixture-session"}},
	}}}})
	for _, test := range []struct{ path, body, want string }{
		{"/v1/chat/completions", `{"model":"demo","max_tokens":20,"messages":[{"role":"user","content":"hello"}]}`, `"content":"ok"`},
		{"/v1/responses", `{"model":"demo","max_output_tokens":20,"input":"hello","stream":true}`, `"type":"response.completed"`},
	} {
		req := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
		result := httptest.NewRecorder()
		server.ServeHTTP(result, req)
		if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), test.want) ||
			!strings.Contains(result.Body.String(), `"total_tokens":4`) {
			t.Fatalf("%s = %d %s", test.path, result.Code, result.Body.String())
		}
	}
}
