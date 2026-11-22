package runtime

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

func googleImageEditPayload(input requestPayload, candidate Candidate) (map[string]any, error) {
	values := clone(input.fields)
	var parts, references []map[string]any
	var mask []byte
	for _, part := range input.form {
		switch part.name {
		case "image", "image[]", "images":
			encoded := base64.StdEncoding.EncodeToString(part.body)
			media := part.header.Get("Content-Type")
			if media == "" || media == "application/octet-stream" {
				media = http.DetectContentType(part.body)
			}
			if len(part.body) == 0 || !strings.HasPrefix(media, "image/") {
				return nil, errors.New("invalid image upload")
			}
			parts = append(parts, map[string]any{"inlineData": map[string]string{"mimeType": media, "data": encoded}})
			if candidate.Vertex != nil && !strings.Contains(candidate.ModelID, "gemini") {
				references = append(references, map[string]any{"referenceType": "REFERENCE_TYPE_RAW", "referenceId": len(references) + 1,
					"referenceImage": map[string]string{"bytesBase64Encoded": encoded}})
			}
		case "mask":
			mask = part.body
		case "prompt", "size", "aspect_ratio", "aspectRatio", "n":
			values[part.name] = string(part.body)
		}
	}
	if len(parts) == 0 {
		return nil, errors.New("Google image edit requires an uploaded image")
	}
	prompt, _ := values["prompt"].(string)
	ratio := googleImageRatio(values)
	if len(references) != 0 {
		if prompt == "" {
			return nil, errors.New("Vertex Imagen edit requires a prompt")
		}
		if len(mask) > 0 {
			references = append(references, map[string]any{"referenceType": "REFERENCE_TYPE_MASK", "referenceId": len(references) + 1,
				"referenceImage":  map[string]string{"bytesBase64Encoded": base64.StdEncoding.EncodeToString(mask)},
				"maskImageConfig": map[string]any{"maskMode": "MASK_MODE_USER_PROVIDED", "dilation": 0.03}})
		}
		count := 1
		if raw, _ := values["n"].(string); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				return nil, errors.New("n must be a positive image count")
			}
			count = n
		}
		return map[string]any{
			"instances": []map[string]any{{"prompt": prompt, "referenceImages": references}},
			"parameters": map[string]any{"sampleCount": count, "editMode": "EDIT_MODE_INPAINT_INSERTION",
				"editConfig": map[string]int{"baseSteps": 50}, "guidanceScale": 7.5, "seed": nil},
		}, nil
	}
	if prompt != "" {
		parts = append(parts, map[string]any{"text": prompt})
	}
	content := map[string]any{"parts": parts}
	if candidate.Vertex != nil {
		content["role"] = "USER"
		config := map[string]any{"response_modalities": []string{"IMAGE"}}
		if ratio != "" {
			config["image_config"] = map[string]string{"aspect_ratio": ratio}
		}
		return map[string]any{"contents": content, "generationConfig": config}, nil
	}
	result := map[string]any{"contents": []map[string]any{content}}
	if ratio != "" {
		result["generationConfig"] = map[string]any{"imageConfig": map[string]string{"aspectRatio": ratio}}
	}
	return result, nil
}
