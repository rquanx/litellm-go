package runtime

import (
	"encoding/json"
	"strings"
)

func cohereEmbeddingTarget(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v2/embed") {
		return base
	}
	return strings.TrimSuffix(base, "/v2") + "/v2/embed"
}

func cohereEmbeddingRequest(payload map[string]any) map[string]any {
	input := payload["input"]
	if text, ok := input.(string); ok {
		input = []string{text}
	}
	result := map[string]any{"model": payload["model"], "texts": input,
		"input_type": "search_document", "embedding_types": []string{"float"}}
	for _, field := range []string{"input_type", "truncate", "embedding_types"} {
		if value, exists := payload[field]; exists {
			result[field] = value
		}
	}
	if dimensions, exists := payload["dimensions"]; exists {
		result["output_dimension"] = dimensions
	}
	return result
}

func cohereEmbeddingResponse(body []byte, alias string) []byte {
	var response struct {
		Embeddings struct {
			Float []json.RawMessage `json:"float"`
		} `json:"embeddings"`
		Meta struct {
			Billed struct {
				Tokens int `json:"input_tokens"`
			} `json:"billed_units"`
		} `json:"meta"`
	}
	if json.Unmarshal(body, &response) != nil || response.Embeddings.Float == nil {
		return body
	}
	data := make([]map[string]any, 0, len(response.Embeddings.Float))
	for index, vector := range response.Embeddings.Float {
		data = append(data, map[string]any{"index": index, "object": "embedding", "embedding": vector})
	}
	tokens := response.Meta.Billed.Tokens
	result, err := json.Marshal(map[string]any{"data": data, "object": "list", "model": alias,
		"usage": map[string]int{"prompt_tokens": tokens, "completion_tokens": 0, "total_tokens": tokens}})
	if err != nil {
		return body
	}
	return result
}
