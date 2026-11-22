package runtime

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesStreamUsesStableChatToolIndices(t *testing.T) {
	stream := responsesChatStream{alias: "demo", includeUsage: true}
	events := []string{
		`{"type":"response.created","response":{"id":"resp-1","created_at":5}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc-a","call_id":"call-a","name":"lookup","arguments":""}}`,
		`{"type":"response.output_item.added","output_index":3,"item":{"type":"function_call","id":"fc-b","call_id":"call-b","name":"list","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc-b","delta":"[]"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc-a","delta":"{}"}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":8,"output_tokens":5,"total_tokens":13}}}`,
	}
	var result strings.Builder
	for _, event := range events {
		result.Write(stream.convert([]byte("data: " + event + "\n\n")))
	}
	var indexes []int
	var finishReason string
	promptTokens := 0
	for _, line := range strings.Split(result.String(), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var chunk struct {
			ID      string `json:"id"`
			Created int    `json:"created"`
			Choices []struct {
				Finish string `json:"finish_reason"`
				Delta  struct {
					Tools []struct {
						Index int `json:"index"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage struct {
				Prompt int `json:"prompt_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatal(err)
		}
		if chunk.ID != "resp-1" || chunk.Created != 5 {
			t.Fatalf("inconsistent Chat chunk metadata: %s", data)
		}
		if len(chunk.Choices) == 0 {
			promptTokens = chunk.Usage.Prompt
			continue
		}
		choice := chunk.Choices[0]
		for _, tool := range choice.Delta.Tools {
			indexes = append(indexes, tool.Index)
		}
		if choice.Finish != "" {
			finishReason = choice.Finish
		}
	}
	if len(indexes) != 4 || indexes[0] != 0 || indexes[1] != 1 || indexes[2] != 1 || indexes[3] != 0 ||
		finishReason != "tool_calls" || promptTokens != 8 || !strings.HasSuffix(result.String(), "data: [DONE]\n\n") {
		t.Fatalf("unexpected converted stream: %s", result.String())
	}
}
