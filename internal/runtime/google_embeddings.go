package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

func (s *Server) googleEmbeddings(ctx context.Context, request *http.Request, candidate Candidate, input requestPayload) (int, []byte, http.Header, bool, error) {
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
	taskType := input.fields["task_type"]
	if taskType == nil {
		taskType = input.fields["input_type"]
	}
	title := input.fields["title"]
	items := make([]map[string]any, 0, len(texts))
	fields := make(map[string]any)
	if candidate.Vertex == nil {
		model := "models/" + strings.TrimPrefix(candidate.ModelID, "models/")
		for _, text := range texts {
			item := map[string]any{"model": model, "content": map[string]any{"parts": []map[string]string{{"text": text}}}}
			for name, value := range map[string]any{"outputDimensionality": dimensions, "taskType": taskType, "title": title} {
				if value != nil {
					item[name] = value
				}
			}
			items = append(items, item)
		}
		fields["requests"] = items
	} else {
		for _, text := range texts {
			item := map[string]any{"content": text}
			if taskType != nil {
				item["task_type"] = taskType
			}
			if title != nil {
				item["title"] = title
			}
			items = append(items, item)
		}
		fields["instances"] = items
		if dimensions != nil {
			fields["parameters"] = map[string]any{"outputDimensionality": dimensions}
		}
	}
	status, response, upstreamHeaders, retryable, err := s.callProviderJSON(ctx, request, "embeddings", candidate, requestPayload{fields: fields})
	if err != nil || status < 200 || status >= 300 {
		return status, response, upstreamHeaders, retryable, err
	}
	type vector struct {
		Values json.RawMessage `json:"values"`
		Stats struct {
			Tokens int `json:"token_count"`
		} `json:"statistics"`
	}
	var result struct {
		Embeddings []vector `json:"embeddings"`
		Predictions []struct {
			Embedding vector `json:"embeddings"`
		} `json:"predictions"`
		Usage struct {
			Tokens int `json:"promptTokenCount"`
		} `json:"usageMetadata"`
	}
	if json.Unmarshal(response, &result) != nil {
		return 0, nil, nil, true, errors.New("invalid Google embedding response")
	}
	total := result.Usage.Tokens
	vectors := result.Embeddings
	if candidate.Vertex != nil {
		total = 0
		for _, prediction := range result.Predictions {
			vectors = append(vectors, prediction.Embedding)
			total += prediction.Embedding.Stats.Tokens
		}
	}
	if len(vectors) != len(texts) {
		return 0, nil, nil, true, errors.New("Google embedding response has an unexpected vector count")
	}
	data := make([]map[string]any, 0, len(vectors))
	for index, embedding := range vectors {
		if len(embedding.Values) == 0 || embedding.Values[0] != '[' {
			return 0, nil, nil, true, errors.New("Google embedding response has no vector")
		}
		data = append(data, map[string]any{"object": "embedding", "index": index, "embedding": embedding.Values})
	}
	body, err := json.Marshal(map[string]any{"object": "list", "model": candidate.ModelID, "data": data,
		"usage": map[string]int{"prompt_tokens": total, "completion_tokens": 0, "total_tokens": total}})
	if int64(len(body)) > s.config.MaxBodyBytes {
		return 0, nil, nil, false, errors.New("Google embedding response exceeds the body limit")
	}
	return http.StatusOK, body, headers, false, err
}
