package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
)

func (s *Server) bedrockCohereEmbeddings(ctx context.Context, request *http.Request, candidate Candidate,
	input map[string]any, texts []string) (int, []byte, http.Header, bool, error) {
	params := map[string]any{"texts": texts, "input_type": "search_document"}
	if strings.HasPrefix(texts[len(texts)-1], "data:image/") {
		delete(params, "texts")
		params["images"], params["input_type"] = texts, "image"
	}
	for _, key := range []string{"input_type", "truncate", "embedding_types", "output_dimension"} {
		if value := input[key]; value != nil {
			params[key] = value
		}
	}
	if dimensions := input["dimensions"]; dimensions != nil {
		params["output_dimension"] = dimensions
	}
	if format, ok := input["encoding_format"].(string); ok && format != "" {
		params["embedding_types"] = []string{format}
	}
	status, raw, headers, retryable, err := s.callProviderJSON(ctx, request, "embeddings", candidate, requestPayload{fields: params})
	if err != nil || status/100 != 2 {
		return status, raw, headers, retryable, err
	}
	var provider struct {
		Embeddings json.RawMessage `json:"embeddings"`
		Meta struct {
			Billed struct {
				Text *int `json:"input_tokens"`
				Images *int `json:"images"`
			} `json:"billed_units"`
		} `json:"meta"`
	}
	if json.Unmarshal(raw, &provider) != nil || len(provider.Embeddings) == 0 {
		return 0, nil, nil, true, errors.New("invalid Bedrock Cohere embedding response")
	}
	var vectors []json.RawMessage
	if json.Unmarshal(provider.Embeddings, &vectors) != nil {
		var types map[string][]json.RawMessage
		if json.Unmarshal(provider.Embeddings, &types) != nil || len(types) == 0 {
			return 0, nil, nil, true, errors.New("Bedrock Cohere response has no embeddings")
		}
		keys := make([]string, 0, len(types))
		for key := range types {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		vectors = types[keys[0]]
		if requested, ok := params["embedding_types"].([]string); ok && len(requested) > 0 && types[requested[0]] != nil {
			vectors = types[requested[0]]
		} else if types["float"] != nil {
			vectors = types["float"]
		}
	}
	if len(vectors) != len(texts) {
		return 0, nil, nil, true, errors.New("Bedrock Cohere returned the wrong number of embeddings")
	}
	data := make([]map[string]any, 0, len(vectors))
	for i, vector := range vectors {
		data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": vector})
	}
	var tokens int
	billed := provider.Meta.Billed
	usage := map[string]any{"completion_tokens": 0}
	if billed.Text != nil || billed.Images != nil {
		if billed.Text != nil {
			tokens += *billed.Text
		}
		if billed.Images != nil {
			tokens += *billed.Images
		}
		usage["prompt_tokens_details"] = map[string]*int{"text_tokens": billed.Text, "image_tokens": billed.Images}
	} else if codec, err := tokenizerForModel(candidate.ModelID); err == nil {
		for _, text := range texts {
			count, _ := codec.Count(text)
			tokens += count
		}
	}
	usage["prompt_tokens"], usage["total_tokens"] = tokens, tokens
	body, err := json.Marshal(map[string]any{"object": "list", "model": candidate.ModelID, "data": data, "usage": usage})
	if int64(len(body)) > s.config.MaxBodyBytes {
		return 0, nil, nil, false, errors.New("normalized Bedrock Cohere embeddings exceed the body limit")
	}
	return status, body, headers, false, err
}
