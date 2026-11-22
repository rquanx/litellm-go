package runtime

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestLiteLLMFormatParity(t *testing.T) {
	fixture, err := os.ReadFile("testdata/litellm-formats.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name      string
		Direction string
		Input     map[string]any
		Output    any
	}
	if err := json.Unmarshal(fixture, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("missing parity cases")
	}
	for _, test := range cases {
		t.Run(test.Direction+"/"+test.Name, func(t *testing.T) {
			var request map[string]any
			var inputKey, outputKey string
			switch test.Direction {
			case "responses_to_chat":
				inputKey, outputKey = "text", "response_format"
				request = responsesToChat(map[string]any{"input": "hello", inputKey: test.Input})
			case "chat_to_responses":
				inputKey, outputKey = "response_format", "text"
				messages := []any{map[string]any{"role": "user", "content": "hello"}}
				request = chatRequestToResponses(map[string]any{"messages": messages, inputKey: test.Input})
			default:
				t.Fatalf("unknown direction %q", test.Direction)
			}
			output, present := request[outputKey]
			if present != (test.Output != nil) || !reflect.DeepEqual(output, test.Output) {
				t.Fatalf("%s = %#v, want %#v", outputKey, output, test.Output)
			}
			if _, present := request[inputKey]; present {
				t.Fatalf("source parameter %s leaked into the target request", inputKey)
			}
		})
	}
}

func TestResponsesFunctionToolToChat(t *testing.T) {
	request := responsesToChat(map[string]any{
		"input": "hello",
		"tools": []any{map[string]any{
			"type": "function", "name": "lookup", "strict": true,
			"parameters": map[string]any{"properties": map[string]any{}},
		}},
		"tool_choice": map[string]any{"type": "function", "name": "lookup"},
	})
	wantTools := []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "lookup", "description": "", "strict": true,
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}}
	if !reflect.DeepEqual(request["tools"], wantTools) {
		t.Fatalf("tools = %#v, want %#v", request["tools"], wantTools)
	}
	wantChoice := map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}
	if !reflect.DeepEqual(request["tool_choice"], wantChoice) {
		t.Fatalf("tool_choice = %#v, want %#v", request["tool_choice"], wantChoice)
	}
}

func TestCrossProtocolUsage(t *testing.T) {
	chat := map[string]any{
		"prompt_tokens": 8, "completion_tokens": 5, "total_tokens": 13,
		"prompt_tokens_details": map[string]any{"cached_tokens": 2},
		"completion_tokens_details": map[string]any{"reasoning_tokens": 1},
	}
	responses := map[string]any{
		"input_tokens": 8, "output_tokens": 5, "total_tokens": 13,
		"input_tokens_details": map[string]any{"cached_tokens": 2},
		"output_tokens_details": map[string]any{"reasoning_tokens": 1},
	}
	if got := chatToResponses(map[string]any{"usage": chat}, "demo")["usage"]; !reflect.DeepEqual(got, responses) {
		t.Fatalf("Responses usage = %#v, want %#v", got, responses)
	}
	if got := responsesToChatResponse(map[string]any{"usage": responses}, "demo")["usage"]; !reflect.DeepEqual(got, chat) {
		t.Fatalf("Chat usage = %#v, want %#v", got, chat)
	}
	if got := chatToResponses(nil, "demo")["usage"]; got != nil {
		t.Fatalf("missing usage was replaced with %#v", got)
	}
}

func TestCrossProtocolImageAndParallelToolHistory(t *testing.T) {
	chatParts := []any{
		map[string]any{"type": "text", "text": "Describe the image"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://fixture.invalid/image.png", "detail": "low"}},
	}
	responsesParts := []any{
		map[string]any{"type": "input_text", "text": "Describe the image"},
		map[string]any{"type": "input_image", "image_url": "https://fixture.invalid/image.png", "detail": "low"},
	}
	message := map[string]any{"role": "user", "content": chatParts}
	responses := chatRequestToResponses(map[string]any{"messages": []any{message}})
	input := responses["input"].([]any)
	if actual := input[0].(map[string]any)["content"]; !reflect.DeepEqual(actual, responsesParts) {
		t.Fatalf("Responses content = %#v, want %#v", actual, responsesParts)
	}
	input = append(input,
		map[string]any{"type": "function_call", "call_id": "call-a", "name": "lookup", "arguments": "{}"},
		map[string]any{"type": "function_call", "call_id": "call-b", "name": "list", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "call-a", "output": "a"},
		map[string]any{"type": "function_call_output", "call_id": "call-b", "output": "b"},
	)
	chat := responsesToChat(map[string]any{"input": input})
	messages := chat["messages"].([]any)
	if len(messages) != 4 || !reflect.DeepEqual(messages[0], message) {
		t.Fatalf("unexpected message history: %#v", messages)
	}
	calls, _ := messages[1].(map[string]any)["tool_calls"].([]any)
	if len(calls) != 2 || messages[3].(map[string]any)["tool_call_id"] != "call-b" {
		t.Fatalf("parallel calls or results changed: %#v", messages)
	}
}
