package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

func marengoEmbeddingRequest(input map[string]any, text, kind string) map[string]any {
	params := map[string]any{"inputType": kind}
	for _, name := range []string{"embeddingOption", "textTruncate", "startSec", "endSec", "segmentDurationSec"} {
		if value := input[name]; value != nil {
			params[name] = value
		}
	}
	if params["embeddingOption"] == nil && input["encoding_format"] == "base64" {
		params["embeddingOption"] = []string{"visual-text", "visual-image"}
	}
	if kind == "text" {
		params["inputText"] = text
		if params["textTruncate"] == nil {
			params["textTruncate"] = "end"
		}
	} else if strings.HasPrefix(text, "s3://") {
		location := map[string]any{"uri": text}
		if owner := input["bucketOwner"]; owner != nil {
			location["bucketOwner"] = owner
		}
		params["mediaSource"] = map[string]any{"s3Location": location}
	} else {
		if _, encoded, ok := strings.Cut(text, ","); ok && strings.HasPrefix(text, "data:") {
			text = encoded
		}
		params["mediaSource"] = map[string]any{"base64String": text}
	}
	return params
}

func (s *Server) bedrockMarengoEmbeddings(ctx context.Context, request *http.Request, candidate Candidate,
	input map[string]any, texts []string) (int, []byte, http.Header, bool, error) {
	kind, _ := input["inputType"].(string)
	if kind == "" {
		kind, _ = input["input_type"].(string)
	}
	if kind == "" {
		kind = "text"
	}
	model, async := strings.CutPrefix(candidate.ModelID, "async_invoke/")
	headers := http.Header{"Content-Type": {"application/json"}}
	if kind != "text" && kind != "image" && (!async || (kind != "audio" && kind != "video")) {
		return http.StatusBadRequest, []byte(`{"error":{"message":"Unsupported Marengo input type for this invocation"}}`), headers, false, nil
	}
	if async {
		return s.startBedrockEmbedding(ctx, request, candidate, model, input, texts, marengoEmbeddingRequest(input, texts[0], kind))
	}
	type vector struct {
		Value []json.RawMessage `json:"embedding"`
		Tokens *int `json:"inputTextTokenCount"`
	}
	data := make([]map[string]any, 0, len(texts))
	var total int
	var bytes int64
	for _, text := range texts {
		payload := marengoEmbeddingRequest(input, text, kind)
		status, raw, upstreamHeaders, retryable, err := s.callProviderJSON(ctx, request, "embeddings", candidate, requestPayload{fields: payload})
		if err != nil || status/100 != 2 {
			return status, raw, upstreamHeaders, retryable, err
		}
		var response struct {
			vector
			Data []vector `json:"data"`
			Embeddings []vector `json:"embeddings"`
		}
		if json.Unmarshal(raw, &response) != nil {
			return 0, nil, nil, true, errors.New("invalid Bedrock Marengo response")
		}
		items := response.Data
		if items == nil {
			items = response.Embeddings
		}
		if items == nil && response.Value != nil {
			items = []vector{response.vector}
		}
		for _, item := range items {
			if item.Value == nil {
				continue
			}
			if item.Tokens != nil {
				total += *item.Tokens
			} else {
				total += len(item.Value)/4
			}
			data = append(data, map[string]any{"object": "embedding", "index": len(data), "embedding": item.Value})
		}
		bytes += int64(len(raw))
		if bytes > s.config.MaxBodyBytes {
			return 0, nil, nil, false, errors.New("aggregated Bedrock Marengo embeddings exceed the body limit")
		}
	}
	body, err := json.Marshal(map[string]any{"object": "list", "model": model, "data": data,
		"usage": map[string]int{"prompt_tokens": total, "completion_tokens": 0, "total_tokens": total}})
	if int64(len(body)) > s.config.MaxBodyBytes {
		return 0, nil, nil, false, errors.New("aggregated Bedrock Marengo embeddings exceed the body limit")
	}
	return http.StatusOK, body, headers, false, err
}
