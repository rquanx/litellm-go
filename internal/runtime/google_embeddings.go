package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

func (s *Server) googleEmbeddings(ctx context.Context, request *http.Request, candidate Candidate, input requestPayload) (int, []byte, http.Header, bool, error) {
	model := strings.TrimPrefix(candidate.ModelID, "models/")
	if candidate.Vertex != nil && (model == "multimodalembedding" || model == "multimodalembedding@001") {
		return s.vertexMultimodalEmbeddings(ctx, request, candidate, input.fields)
	}
	if candidate.Vertex == nil || VertexUsesEmbedContent(model) {
		return s.geminiContentEmbeddings(ctx, request, candidate, model, input.fields)
	}
	var texts []string
	switch value := input.fields["input"].(type) {
	case string:
		texts = []string{value}
	case []any:
		for _, item := range value {
			text, ok := item.(string)
			if !ok {
				texts = nil
				break
			}
			texts = append(texts, text)
		}
	}
	headers := http.Header{"Content-Type": {"application/json"}}
	if len(texts) == 0 {
		return http.StatusBadRequest, []byte(`{"error":{"code":"invalid_request","message":"Supply a string or a list of strings"}}`), headers, false, nil
	}
	dimensions := input.fields["dimensions"]
	bge := strings.Contains(strings.ToLower(model), "bge")
	fineTuned := vertexNumericEndpoint(model)
	taskType := input.fields["task_type"]
	if taskType == nil {
		taskType = input.fields["input_type"]
	}
	title := input.fields["title"]
	items := make([]map[string]any, 0, len(texts))
	fields := make(map[string]any)
	field := "content"
	switch {
	case bge:
		field = "prompt"
	case fineTuned:
		field = "inputs"
	}
	for _, text := range texts {
		item := map[string]any{field: text}
		if fineTuned {
			items = append(items, item)
			continue
		}
		if taskType != nil {
			item["task_type"] = taskType
		}
		if title != nil {
			item["title"] = title
		}
		items = append(items, item)
	}
	fields["instances"] = items
	parameters := make(map[string]any)
	if dimensions != nil {
		parameters["outputDimensionality"] = dimensions
	}
	if truncate := input.fields["auto_truncate"]; truncate != nil {
		parameters["auto_truncate"] = truncate
	}
	if bge || fineTuned {
		for name, value := range map[string]any{"task_type": taskType, "title": title} {
			if value != nil {
				parameters[name] = value
			}
		}
	}
	if fineTuned {
		for _, name := range []string{"max_new_tokens", "temperature", "top_p", "top_k"} {
			if value := input.fields[name]; value != nil {
				parameters[name] = value
			}
		}
	}
	if len(parameters) != 0 || bge || fineTuned {
		fields["parameters"] = parameters
	}
	status, response, upstreamHeaders, retryable, err := s.callProviderJSON(ctx, request, "embeddings", candidate, requestPayload{fields: fields})
	if err != nil || status < 200 || status >= 300 {
		return status, response, upstreamHeaders, retryable, err
	}
	type vector struct {
		Values json.RawMessage `json:"values"`
		Stats  struct {
			Tokens int `json:"token_count"`
		} `json:"statistics"`
	}
	var result struct {
		Predictions []json.RawMessage `json:"predictions"`
	}
	if json.Unmarshal(response, &result) != nil {
		return 0, nil, nil, true, errors.New("invalid Google embedding response")
	}
	total := 0
	vectors := make([]json.RawMessage, 0, len(result.Predictions))
	for _, raw := range result.Predictions {
		var embedding vector
		switch {
		case bge:
			embedding.Values = raw
		case fineTuned:
			var values []json.RawMessage
			if json.Unmarshal(raw, &values) != nil || len(values) == 0 {
				return 0, nil, nil, true, errors.New("invalid fine-tuned Vertex embedding prediction")
			}
			embedding.Values = values[0]
		default:
			var prediction struct {
				Embedding vector `json:"embeddings"`
			}
			if json.Unmarshal(raw, &prediction) != nil {
				return 0, nil, nil, true, errors.New("invalid Vertex embedding prediction")
			}
			embedding = prediction.Embedding
		}
		vectors = append(vectors, embedding.Values)
		total += embedding.Stats.Tokens
	}
	return s.googleEmbeddingResult(candidate.ModelID, vectors, len(texts), total)
}

func (s *Server) googleEmbeddingResult(model string, vectors []json.RawMessage, count, tokens int) (int, []byte, http.Header, bool, error) {
	if len(vectors) != count {
		return 0, nil, nil, true, errors.New("Google embedding response has an unexpected vector count")
	}
	data := make([]map[string]any, 0, len(vectors))
	for index, embedding := range vectors {
		if len(embedding) == 0 || embedding[0] != '[' {
			return 0, nil, nil, true, errors.New("Google embedding response has no vector")
		}
		data = append(data, map[string]any{"object": "embedding", "index": index, "embedding": embedding})
	}
	body, err := json.Marshal(map[string]any{"object": "list", "model": model, "data": data,
		"usage": map[string]int{"prompt_tokens": tokens, "completion_tokens": 0, "total_tokens": tokens}})
	if int64(len(body)) > s.config.MaxBodyBytes {
		return 0, nil, nil, false, errors.New("Google embedding response exceeds the body limit")
	}
	return http.StatusOK, body, http.Header{"Content-Type": {"application/json"}}, false, err
}

func vertexNumericEndpoint(model string) bool {
	return model != "" && strings.Trim(model, "0123456789") == ""
}
