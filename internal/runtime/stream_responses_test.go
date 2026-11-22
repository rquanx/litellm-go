package runtime

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestChatStreamPreservesParallelFunctionArguments(t *testing.T) {
	stream := chatResponsesStream{alias: "demo", limit: 1024}
	chunks := []string{
		`{"id":"chat-1","created":1,"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-a","function":{"name":"lookup","arguments":"{\"id\":"}},{"index":1,"id":"call-b","function":{"name":"list","arguments":"["}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"1]"}},{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":8,"completion_tokens":5,"total_tokens":13}}`,
		"[DONE]",
	}
	arguments := map[string]string{}
	completed := 0
	for _, chunk := range chunks {
		converted := stream.convert([]byte("data: " + chunk + "\n\n"))
		for _, line := range strings.Split(string(converted), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event struct {
				Type     string `json:"type"`
				ItemID   string `json:"item_id"`
				Delta    string `json:"delta"`
				Response struct {
					Output []struct {
						CallID    string `json:"call_id"`
						Arguments string `json:"arguments"`
					} `json:"output"`
					Usage struct {
						Input int `json:"input_tokens"`
					} `json:"usage"`
				} `json:"response"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				t.Fatal(err)
			}
			switch event.Type {
			case "response.function_call_arguments.delta":
				arguments[event.ItemID] += event.Delta
			case "response.completed":
				completed++
				if chunk != "[DONE]" || event.Response.Usage.Input != 8 || len(event.Response.Output) != 2 {
					t.Fatalf("unexpected terminal event: %s", line)
				}
				for _, item := range event.Response.Output {
					if item.Arguments != arguments[item.CallID] {
						t.Fatalf("final arguments disagree with deltas: %s", line)
					}
				}
			}
		}
	}
	want := map[string]string{"call-a": `{"id":1}`, "call-b": "[1]"}
	if completed != 1 || !reflect.DeepEqual(arguments, want) {
		t.Fatalf("completed=%d arguments=%v, want %v", completed, arguments, want)
	}
}
