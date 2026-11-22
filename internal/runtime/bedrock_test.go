package runtime

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

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
