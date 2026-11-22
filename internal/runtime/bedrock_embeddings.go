package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

func (s *Server) bedrockEmbeddings(ctx context.Context, request *http.Request, candidate Candidate, input requestPayload) (int, []byte, http.Header, bool, error) {
	if timeout := s.config.HTTPClient.Timeout; timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var texts []string
	switch value := input.fields["input"].(type) {
	case string:
		texts = []string{value}
	case []any:
		if len(value) <= 2048 {
			for _, item := range value {
				text, ok := item.(string)
				if !ok {
					texts = nil
					break
				}
				texts = append(texts, text)
			}
		}
	}
	headers := http.Header{"Content-Type": {"application/json"}}
	if len(texts) == 0 {
		return http.StatusBadRequest, []byte(`{"error":{"code":"invalid_request","message":"Supply a string or a list of strings"}}`), headers, false, nil
	}
	base := make(map[string]any)
	encoding, _ := input.fields["encoding_format"].(string)
	if strings.Contains(candidate.ModelID, "embed-text-v2") {
		for _, name := range []string{"dimensions", "normalize"} {
			if value, exists := input.fields[name]; exists {
				base[name] = value
			}
		}
		if encoding == "base64" {
			base["embeddingTypes"] = []string{"binary"}
		} else if encoding != "" {
			base["embeddingTypes"] = []string{"float"}
		}
	}
	data := make([]map[string]any, 0, len(texts))
	total := 0
	var vectorBytes int64
	for index, text := range texts {
		base["inputText"] = text
		status, response, upstreamHeaders, retryable, err := s.callProviderJSON(ctx, request, "embeddings", candidate, requestPayload{fields: base})
		if err != nil || status < 200 || status >= 300 {
			return status, response, upstreamHeaders, retryable, err
		}
		var result struct {
			Embedding json.RawMessage `json:"embedding"`
			Types map[string]json.RawMessage `json:"embeddingsByType"`
			Tokens int `json:"inputTextTokenCount"`
		}
		if json.Unmarshal(response, &result) != nil {
			return 0, nil, nil, true, errors.New("invalid Bedrock embedding response")
		}
		vector := result.Embedding
		if len(result.Types["binary"]) > 0 {
			vector = result.Types["binary"]
		} else if len(result.Types["float"]) > 0 {
			vector = result.Types["float"]
		}
		if len(vector) == 0 || vector[0] != '[' {
			return 0, nil, nil, true, errors.New("Bedrock embedding response has no vector")
		}
		vectorBytes += int64(len(vector))
		if vectorBytes > s.config.MaxBodyBytes {
			return 0, nil, nil, false, errors.New("aggregated Bedrock embedding response exceeds the body limit")
		}
		data = append(data, map[string]any{"object": "embedding", "index": index, "embedding": vector})
		total += result.Tokens
	}
	body, err := json.Marshal(map[string]any{"object": "list", "model": candidate.ModelID, "data": data,
		"usage": map[string]int{"prompt_tokens": total, "completion_tokens": 0, "total_tokens": total}})
	if int64(len(body)) > s.config.MaxBodyBytes {
		return 0, nil, nil, false, errors.New("aggregated Bedrock embedding response exceeds the body limit")
	}
	return http.StatusOK, body, headers, false, err
}
