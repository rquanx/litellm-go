package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type invalidGoogleImageRequest struct{ error }

func googleImageDeployment(candidate Candidate) bool {
	return (candidate.Endpoint == "image-generation" || candidate.Endpoint == "image-edit") && (candidate.Provider == "gemini" || candidate.Provider == "vertex_ai")
}

func googleImageTarget(candidate Candidate) string {
	target := geminiTarget(candidate, "gemini-generate")
	if !strings.Contains(candidate.ModelID, "gemini") && (candidate.Endpoint != "image-edit" || candidate.Vertex != nil) {
		target = strings.TrimSuffix(target, ":generateContent") + ":predict"
	}
	return target
}

func googleImagePayload(input map[string]any, candidate Candidate) (map[string]any, error) {
	prompt, _ := input["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("image generation requires a prompt")
	}
	ratio := googleImageRatio(input)
	n, hasN := input["n"].(float64)
	if input["n"] != nil && (!hasN || n < 1 || n > 100 || n != float64(int(n))) {
		return nil, errors.New("n must be a positive image count")
	}
	return googleImageGenerationPayload(input, candidate, prompt, ratio, n, hasN), nil
}

func googleImageRatio(input map[string]any) string {
	size, _ := input["size"].(string)
	ratio := map[string]string{"1024x1024": "1:1", "1792x1024": "16:9", "1024x1792": "9:16", "1280x896": "4:3", "896x1280": "3:4"}[size]
	if size != "" && ratio == "" {
		ratio = "1:1"
	}
	if custom, _ := input["aspect_ratio"].(string); custom != "" {
		ratio = custom
	}
	if custom, _ := input["aspectRatio"].(string); custom != "" {
		ratio = custom
	}
	return ratio
}

func googleImageGenerationPayload(input map[string]any, candidate Candidate, prompt, ratio string, n float64, hasN bool) map[string]any {
	vertex := candidate.Vertex != nil
	if !strings.Contains(candidate.ModelID, "gemini") {
		parameters := make(map[string]any)
		if vertex {
			parameters["sampleCount"] = 1
		}
		if hasN {
			parameters["sampleCount"] = n
		}
		if ratio != "" {
			parameters["aspectRatio"] = ratio
		}
		for _, field := range []string{"personGeneration", "safetySetting", "negativePrompt", "addWatermark", "outputOptions", "imageSize"} {
			if value := input[field]; value != nil {
				parameters[field] = value
			}
		}
		result := map[string]any{"instances": []map[string]string{{"prompt": prompt}}, "parameters": parameters}
		if labels, ok := input["labels"].(map[string]any); vertex && ok {
			result["labels"] = labels
		}
		return result
	}
	content := map[string]any{"parts": []map[string]string{{"text": prompt}}}
	config := map[string]any{"response_modalities": []string{"IMAGE", "TEXT"}}
	if vertex {
		content["role"] = "user"
		config = map[string]any{"responseModalities": []string{"IMAGE"}}
		imageConfig := make(map[string]string)
		if ratio != "" {
			imageConfig["aspectRatio"] = ratio
		}
		for _, name := range []string{"image_size", "imageSize"} {
			if value, _ := input[name].(string); value != "" {
				imageConfig["imageSize"] = value
			}
		}
		if len(imageConfig) > 0 {
			config["imageConfig"] = imageConfig
		}
		if hasN {
			config["candidateCount"] = n
		}
		if value, ok := input["candidate_count"].(float64); ok {
			config["candidateCount"] = value
		}
	}
	return map[string]any{"contents": []map[string]any{content}, "generationConfig": config}
}

func googleImageResponse(raw []byte) ([]byte, error) {
	var payload struct {
		Predictions []struct {
			Data string `json:"bytesBase64Encoded"`
		} `json:"predictions"`
		Candidates []struct {
			Content struct {
				Parts []struct {
					Inline struct {
						Data string `json:"data"`
					} `json:"inlineData"`
					Thought string `json:"thoughtSignature"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		Usage *struct {
			Input   int `json:"promptTokenCount"`
			Output  int `json:"candidatesTokenCount"`
			Total   int `json:"totalTokenCount"`
			Details []struct {
				Mode      string `json:"modality"`
				Count     int    `json:"tokenCount"`
				Alternate int    `json:"token_count"`
			} `json:"promptTokensDetails"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("invalid Google image response: %w", err)
	}
	data := make([]map[string]any, 0)
	for _, prediction := range payload.Predictions {
		if prediction.Data != "" {
			data = append(data, map[string]any{"b64_json": prediction.Data})
		}
	}
	for _, candidate := range payload.Candidates {
		for _, part := range candidate.Content.Parts {
			if part.Inline.Data != "" {
				entry := map[string]any{"b64_json": part.Inline.Data}
				if part.Thought != "" {
					entry["provider_specific_fields"] = map[string]string{"thought_signature": part.Thought}
				}
				data = append(data, entry)
			}
		}
	}
	result := map[string]any{"created": time.Now().Unix(), "data": data}
	if usage := payload.Usage; usage != nil {
		details := map[string]int{"text_tokens": 0, "image_tokens": 0}
		for _, entry := range usage.Details {
			switch strings.ToUpper(entry.Mode) {
			case "TEXT":
				details["text_tokens"] += max(entry.Count, entry.Alternate)
			case "IMAGE":
				details["image_tokens"] += max(entry.Count, entry.Alternate)
			}
		}
		result["usage"] = map[string]any{"input_tokens": usage.Input, "output_tokens": usage.Output,
			"total_tokens": usage.Total, "input_tokens_details": details}
	}
	return json.Marshal(result)
}
