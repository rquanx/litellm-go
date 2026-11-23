package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode"
)

type vertexEmbeddingInstance struct {
	Text  *string        `json:"text,omitempty"`
	Image map[string]any `json:"image,omitempty"`
	Video map[string]any `json:"video,omitempty"`
}

func vertexEmbeddingMedia(text string) vertexEmbeddingInstance {
	if strings.HasPrefix(text, "gs://") {
		media := map[string]any{"gcsUri": text}
		if strings.HasSuffix(strings.ToLower(text), ".mp4") {
			return vertexEmbeddingInstance{Video: media}
		}
		return vertexEmbeddingInstance{Image: media}
	}
	if strings.HasPrefix(text, "data:") {
		_, encoded, ok := strings.Cut(text, ",")
		if ok && encoded != "" && !strings.ContainsAny(encoded, "\r\n") {
			if _, err := base64.StdEncoding.Strict().DecodeString(encoded); err == nil {
				return vertexEmbeddingInstance{Image: map[string]any{"bytesBase64Encoded": encoded}}
			}
		}
	}
	return vertexEmbeddingInstance{Text: &text}
}

func vertexEmbeddingInstances(fields map[string]any) ([]vertexEmbeddingInstance, error) {
	var inputs []any
	explicit := fields["instances"] != nil
	if explicit {
		inputs, _ = fields["instances"].([]any)
	} else {
		switch value := fields["input"].(type) {
		case string:
			inputs = []any{value}
		case []any:
			inputs = value
		}
	}
	if len(inputs) == 0 {
		return nil, errors.New("supply text, media, or Vertex embedding instances")
	}
	instances := make([]vertexEmbeddingInstance, 0, len(inputs))
	for i := 0; i < len(inputs); i++ {
		switch value := inputs[i].(type) {
		case string:
			if explicit {
				return nil, errors.New("instances must contain Vertex embedding objects")
			}
			instance := vertexEmbeddingMedia(value)
			if instance.Text != nil && i+1 < len(inputs) {
				if next, ok := inputs[i+1].(string); ok {
					media := vertexEmbeddingMedia(next)
					if media.Text == nil {
						instance.Image, instance.Video = media.Image, media.Video
						i++
					}
				}
			}
			instances = append(instances, instance)
		case map[string]any:
			var instance vertexEmbeddingInstance
			if text, ok := value["text"].(string); ok {
				instance.Text = &text
			}
			instance.Image, _ = value["image"].(map[string]any)
			instance.Video, _ = value["video"].(map[string]any)
			if instance.Text == nil && len(instance.Image) == 0 && len(instance.Video) == 0 {
				return nil, errors.New("invalid Vertex embedding instance")
			}
			instances = append(instances, instance)
		default:
			return nil, errors.New("invalid Vertex embedding input")
		}
	}
	return instances, nil
}

func (s *Server) vertexMultimodalEmbeddings(ctx context.Context, r *http.Request, candidate Candidate, fields map[string]any) (int, []byte, http.Header, bool, error) {
	headers := http.Header{"Content-Type": {"application/json"}}
	instances, err := vertexEmbeddingInstances(fields)
	if err != nil {
		body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": "invalid_request", "message": err.Error()}})
		return http.StatusBadRequest, body, headers, false, nil
	}
	payload := map[string]any{"instances": instances}
	if dimension := fields["dimensions"]; dimension != nil {
		payload["parameters"] = map[string]any{"dimension": dimension}
	}
	status, response, upstreamHeaders, retryable, err := s.callProviderJSON(ctx, r, "embeddings", candidate, requestPayload{fields: payload})
	if err != nil || status/100 != 2 {
		return status, response, upstreamHeaders, retryable, err
	}
	body, err := vertexMultimodalEmbeddingResponse(response, candidate.ModelID, instances)
	if err != nil {
		return 0, nil, nil, true, err
	}
	if int64(len(body)) > s.config.MaxBodyBytes {
		return 0, nil, nil, false, errors.New("Vertex embedding response exceeds the body limit")
	}
	return http.StatusOK, body, headers, false, nil
}

func vertexMultimodalEmbeddingResponse(body []byte, model string, instances []vertexEmbeddingInstance) ([]byte, error) {
	var result struct {
		Predictions []struct {
			Text  json.RawMessage `json:"textEmbedding"`
			Image json.RawMessage `json:"imageEmbedding"`
			Video []struct {
				Start  float64         `json:"startOffsetSec"`
				End    float64         `json:"endOffsetSec"`
				Vector json.RawMessage `json:"embedding"`
			} `json:"videoEmbeddings"`
		} `json:"predictions"`
	}
	if json.Unmarshal(body, &result) != nil || len(result.Predictions) != len(instances) {
		return nil, errors.New("invalid Vertex multimodal embedding response")
	}
	data := make([]map[string]any, 0, len(instances))
	var duration float64
	for i, prediction := range result.Predictions {
		vector := prediction.Text
		if len(vector) == 0 {
			vector = prediction.Image
		}
		if len(vector) > 0 {
			if vector[0] != '[' {
				return nil, errors.New("invalid Vertex embedding vector")
			}
			data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": vector})
		}
		for _, segment := range prediction.Video {
			duration += segment.End - segment.Start
			if len(vector) == 0 {
				if len(segment.Vector) == 0 || segment.Vector[0] != '[' {
					return nil, errors.New("invalid Vertex video embedding vector")
				}
				data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": segment.Vector})
			}
		}
	}
	var characters, images int
	details := map[string]any{"video_length_seconds": duration}
	for _, instance := range instances {
		if instance.Text != nil && *instance.Text != "" {
			for _, char := range *instance.Text {
				if !unicode.IsSpace(char) && (char < 0x1c || char > 0x1f) {
					characters++
				}
			}
			details["character_count"] = characters
		}
		if len(instance.Image) != 0 {
			images++
		}
	}
	details["image_count"] = images
	return json.Marshal(map[string]any{
		"object": "list", "model": model, "data": data,
		"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0, "prompt_tokens_details": details},
	})
}
