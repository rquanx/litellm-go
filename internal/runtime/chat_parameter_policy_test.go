package runtime

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestChatParameterPolicyOverridesAndNestedDrop(t *testing.T) {
	enabled := true
	candidate := Candidate{ModelID: "fixture", Provider: "openai_like", Endpoint: "chat-completions",
		DropParams: &enabled, SupportedOpenAIParams: []string{"tools"},
		AdditionalDropParams: []string{"tools[*].function.description", "tools[0].function.example"}}
	input := requestPayload{fields: map[string]any{
		"model": "alias", "logprobs": true, "drop_params": false,
		"tools": []any{
			map[string]any{"function": map[string]any{"description": "first", "example": 1}},
			map[string]any{"function": map[string]any{"description": "second", "example": 2}},
		},
	}}
	var unsupported unsupportedProviderParams
	if _, _, err := encodePayload(input, "chat-completions", candidate, false, false); !errors.As(err, &unsupported) {
		t.Fatalf("explicit false should override deployment default: %v", err)
	}
	input.fields["allowed_openai_params"] = []any{"logprobs"}
	body, _, err := encodePayload(input, "chat-completions", candidate, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var transformed map[string]any
	if err := json.Unmarshal(body, &transformed); err != nil {
		t.Fatal(err)
	}
	tools := transformed["tools"].([]any)
	first := tools[0].(map[string]any)["function"].(map[string]any)
	second := tools[1].(map[string]any)["function"].(map[string]any)
	if len(first) != 0 || len(second) != 1 || second["example"] != float64(2) || transformed["logprobs"] != true {
		t.Fatalf("incorrect filtered payload: %s", body)
	}
	original := input.fields["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if len(original) != 2 {
		t.Fatal("provider-specific transformation mutated the original request used by fallback")
	}
}

func TestOpenAIReasoningParams(t *testing.T) {
	candidate := Candidate{Provider: "openai", ModelID: "gpt-5.4", Endpoint: "chat-completions",
		ReasoningCapabilities: OpenAIReasoningCapabilities{None: true, HighExtended: true}}
	fields := map[string]any{"messages": "fixture", "temperature": 0.4, "top_p": 0.5,
		"reasoning_effort": map[string]any{"effort": "none"}, "max_tokens": 20}
	data, _, err := encodePayload(requestPayload{fields: fields}, "chat-completions", candidate, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	if body["temperature"] != 0.4 || body["reasoning_effort"] != "none" || body["max_completion_tokens"] != float64(20) || body["max_tokens"] != nil {
		t.Fatalf("incorrect GPT-5 Chat request: %s", data)
	}
	fields["reasoning_effort"] = "high"
	var unsupported unsupportedProviderParams
	if _, _, err := encodePayload(requestPayload{fields: fields}, "chat-completions", candidate, false, false); !errors.As(err, &unsupported) {
		t.Fatalf("sampling with high effort should fail: %v", err)
	}
	data, _, err = encodePayload(requestPayload{fields: fields}, "chat-completions", candidate, false, true)
	if err != nil {
		t.Fatal(err)
	}
	body = nil
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	if body["temperature"] != nil || body["top_p"] != nil || body["reasoning_effort"] != "high" {
		t.Fatalf("incorrect GPT-5 Chat drop behavior: %s", data)
	}

	candidate.Endpoint = "responses"
	fields = map[string]any{"input": "fixture", "temperature": 0.4,
		"reasoning": map[string]any{"effort": "high"}, "drop_params": true}
	if _, _, err := encodePayload(requestPayload{fields: fields}, "responses", candidate, false, false); !errors.As(err, &unsupported) {
		t.Fatalf("native Responses uses the global drop policy: %v", err)
	}
	fields["additional_drop_params"] = []any{"temperature"}
	data, _, err = encodePayload(requestPayload{fields: fields}, "responses", candidate, false, false)
	if err != nil {
		t.Fatal(err)
	}
	body = nil
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	if body["input"] != "fixture" || body["temperature"] != nil || body["drop_params"] != nil || body["additional_drop_params"] != nil {
		t.Fatalf("incorrect GPT-5 Responses payload: %s", data)
	}
}

func TestOpenAIEmbeddingDimensionPolicy(t *testing.T) {
	candidate := Candidate{Endpoint: "embeddings", Provider: "openai", ModelID: "text-embedding-ada-002"}
	fields := map[string]any{"input": "fixture", "dimensions": 32, "drop_params": true}
	var unsupported unsupportedProviderParams
	if _, _, err := encodePayload(requestPayload{fields: fields}, "embeddings", candidate, false, true); !errors.As(err, &unsupported) {
		t.Fatalf("legacy dimensions must be explicitly allowed or dropped: %v", err)
	}
	fields["allowed_openai_params"] = []any{"dimensions"}
	if _, _, err := encodePayload(requestPayload{fields: fields}, "embeddings", candidate, false, false); err != nil {
		t.Fatal(err)
	}
	fields["additional_drop_params"] = []any{"dimensions"}
	data, _, err := encodePayload(requestPayload{fields: fields}, "embeddings", candidate, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 2 || body["model"] != candidate.ModelID || body["input"] != "fixture" {
		t.Fatalf("incorrect embedding payload: %s", data)
	}
}
