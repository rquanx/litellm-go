package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

func novaEmbeddingRequest(input map[string]any, text string, segmented bool) map[string]any {
	params := map[string]any{"embeddingPurpose": "GENERIC_INDEX", "embeddingDimension": 3072}
	for _, key := range []string{"embeddingPurpose", "embeddingDimension", "text", "image", "video", "audio", "segmentationConfig"} {
		if value := input[key]; value != nil {
			params[key] = value
		}
	}
	if dimensions := input["dimensions"]; dimensions != nil {
		params["embeddingDimension"] = dimensions
	} else if dimensions := input["embedding_dimension"]; dimensions != nil {
		params["embeddingDimension"] = dimensions
	}
	if params["text"] == nil && params["image"] == nil && params["video"] == nil && params["audio"] == nil {
		metadata, data, _ := strings.Cut(text, ",")
		mimeType, _, _ := strings.Cut(strings.TrimPrefix(metadata, "data:"), ";")
		kind, format, _ := strings.Cut(mimeType, "/")
		switch {
		case data != "" && strings.HasPrefix(metadata, "data:") && (kind == "image" || kind == "audio" || kind == "video"):
			if format == "jpg" {
				format = "jpeg"
			}
			params[kind] = map[string]any{"format": format, "source": map[string]string{"bytes": data}}
		case strings.HasPrefix(text, "s3://"):
			params["text"] = map[string]any{"truncationMode": "END", "source": map[string]any{"s3Location": map[string]string{"uri": text}}}
		default:
			params["text"] = map[string]string{"value": text, "truncationMode": "END"}
		}
	}
	result := map[string]any{"schemaVersion": "nova-multimodal-embed-v1"}
	if segmented {
		result["taskType"], result["segmentedEmbeddingParams"] = "SEGMENTED_EMBEDDING", params
	} else {
		result["taskType"], result["singleEmbeddingParams"] = "SINGLE_EMBEDDING", params
	}
	return result
}

func (s *Server) bedrockNovaEmbeddings(ctx context.Context, request *http.Request, candidate Candidate,
	input map[string]any, texts []string) (int, []byte, http.Header, bool, error) {
	if model, enabled := strings.CutPrefix(candidate.ModelID, "async_invoke/"); enabled {
		payload := novaEmbeddingRequest(input, texts[0], true)
		return s.startBedrockEmbedding(ctx, request, candidate, model, input, texts, payload)
	}
	data := make([]map[string]any, 0, len(texts))
	var total, images int
	var bytes int64
	for _, text := range texts {
		payload := novaEmbeddingRequest(input, text, false)
		if params, _ := payload["singleEmbeddingParams"].(map[string]any); params["image"] != nil {
			images++
		}
		status, raw, headers, retryable, err := s.callProviderJSON(ctx, request, "embeddings", candidate, requestPayload{fields: payload})
		if err != nil || status/100 != 2 {
			return status, raw, headers, retryable, err
		}
		var response struct {
			Vector json.RawMessage `json:"embedding"`
			Items []struct {
				Vector    json.RawMessage `json:"embedding"`
				TextChars *int            `json:"truncatedCharLength"`
			} `json:"embeddings"`
		}
		if json.Unmarshal(raw, &response) != nil {
			return 0, nil, nil, true, errors.New("invalid Bedrock Nova embedding response")
		}
		add := func(vector json.RawMessage, count *int) bool {
			var values []json.RawMessage
			if json.Unmarshal(vector, &values) != nil || values == nil {
				return false
			}
			tokens := len(values) / 4
			if count != nil {
				tokens = max(0, *count) / 4
			}
			total += tokens
			bytes += int64(len(vector))
			data = append(data, map[string]any{"object": "embedding", "index": len(data), "embedding": vector})
			return true
		}
		before := len(data)
		for _, item := range response.Items {
			add(item.Vector, item.TextChars)
		}
		if len(data) == before && !add(response.Vector, nil) {
			return 0, nil, nil, true, errors.New("Bedrock Nova response has no embeddings")
		}
		if bytes > s.config.MaxBodyBytes {
			return 0, nil, nil, false, errors.New("aggregated Bedrock Nova embeddings exceed the body limit")
		}
	}
	usage := map[string]any{"prompt_tokens": total, "completion_tokens": 0, "total_tokens": total}
	if images > 0 {
		usage["prompt_tokens_details"] = map[string]int{"image_count": images}
	}
	body, err := json.Marshal(map[string]any{"object": "list", "model": candidate.ModelID, "data": data, "usage": usage})
	if int64(len(body)) > s.config.MaxBodyBytes {
		return 0, nil, nil, false, errors.New("aggregated Bedrock Nova embeddings exceed the body limit")
	}
	return http.StatusOK, body, http.Header{"Content-Type": {"application/json"}}, false, err
}

func (s *Server) startBedrockEmbedding(ctx context.Context, request *http.Request, candidate Candidate, model string,
	input map[string]any, texts []string, modelInput map[string]any) (int, []byte, http.Header, bool, error) {
	output, _ := input["output_s3_uri"].(string)
	bucket, err := url.Parse(output)
	headers := http.Header{"Content-Type": {"application/json"}}
	if err != nil || bucket.Scheme != "s3" || bucket.Host == "" || bucket.User != nil || len(texts) != 1 {
		return http.StatusBadRequest, []byte(`{"error":{"message":"Bedrock async embedding requires one input and an S3 output URI"}}`), headers, false, nil
	}
	payload := map[string]any{
		"modelId": model, "modelInput": modelInput,
		"outputDataConfig": map[string]any{"s3OutputDataConfig": map[string]string{"s3Uri": output}},
	}
	status, raw, upstreamHeaders, _, err := s.callProviderJSON(ctx, request, "embeddings", candidate, requestPayload{fields: payload})
	if err != nil || status/100 != 2 {
		return status, raw, upstreamHeaders, false, err
	}
	var result struct{ ARN string `json:"invocationArn"` }
	if json.Unmarshal(raw, &result) != nil || result.ARN == "" {
		return 0, nil, nil, false, errors.New("missing Bedrock invocation ID")
	}
	headers.Set("X-LiteLLM-Invocation-ARN", result.ARN)
	body, err := json.Marshal(map[string]any{"object": "list", "model": candidate.ModelID,
		"data": []map[string]any{{"object": "embedding", "index": 0, "embedding": []float64{}}},
		"usage": map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}})
	return http.StatusOK, body, headers, false, err
}
