package runtime

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

func BedrockSupportsImage(model, endpoint string) bool {
	model = strings.ToLower(model)
	nova := strings.Contains(model, "amazon.nova-canvas")
	stability := strings.Contains(model, "stability.")
	stabilityEdit := stability && slices.ContainsFunc([]string{"upscale", "outpaint", "inpaint", "erase", "remove-background",
		"search-recolor", "search-replace", "control-sketch", "control-structure", "style-guide", "style-transfer"},
		func(operation string) bool { return strings.Contains(model, operation) })
	switch endpoint {
	case "image-edit":
		return nova || stabilityEdit
	case "image-generation":
		return nova || strings.Contains(model, "amazon.titan-image") || (stability && !stabilityEdit)
	default:
		return false
	}
}

func bedrockImageDeployment(candidate Candidate) bool {
	return candidate.Provider == "bedrock" && BedrockSupportsImage(candidate.ModelID, candidate.Endpoint)
}

func bedrockImagePayload(input requestPayload, candidate Candidate) (map[string]any, error) {
	edit := candidate.Endpoint == "image-edit"
	if !strings.Contains(candidate.ModelID, "stability.") {
		return bedrockCanvasPayload(input, edit, strings.Contains(candidate.ModelID, "amazon.titan-image"))
	}
	if edit {
		return stabilityBedrockEdit(input, candidate.ModelID)
	}
	fields := input.fields
	prompt, _ := fields["prompt"].(string)
	if prompt == "" {
		return nil, errors.New("Bedrock Stability generation requires a prompt")
	}
	if strings.Contains(candidate.ModelID, "sd3") || strings.Contains(candidate.ModelID, "stable-image") {
		body := map[string]any{"prompt": prompt}
		for _, name := range []string{"aspect_ratio", "mode", "output_format", "seed", "negative_prompt", "image", "strength"} {
			if fields[name] != nil {
				body[name] = fields[name]
			}
		}
		return body, nil
	}
	body := map[string]any{"text_prompts": []map[string]any{{"text": strings.ReplaceAll(prompt, "\n", " "), "weight": 1}}}
	for _, name := range []string{"cfg_scale", "seed", "steps", "width", "height", "style_preset", "sampler",
		"samples", "clip_guidance_preset", "init_image", "image_strength", "start_schedule", "end_schedule"} {
		if fields[name] != nil {
			body[name] = fields[name]
		}
	}
	if size, _ := fields["size"].(string); size != "" {
		width, height, _ := strings.Cut(size, "x")
		w, errW := strconv.Atoi(width)
		h, errH := strconv.Atoi(height)
		if errW != nil || errH != nil || w < 1 || h < 1 {
			return nil, errors.New("Bedrock Stability size must contain a positive width and height")
		}
		body["width"], body["height"] = w, h
	}
	return body, nil
}

func stabilityBedrockEdit(input requestPayload, model string) (map[string]any, error) {
	fields := clone(input.fields)
	for _, part := range input.form {
		if slices.Contains([]string{"image", "mask", "init_image", "style_image"}, part.name) {
			fields[part.name] = base64.StdEncoding.EncodeToString(part.body)
		} else {
			fields[part.name] = string(part.body)
		}
	}
	body := map[string]any{"output_format": "png"}
	if size, _ := fields["size"].(string); size != "" {
		if ratio := map[string]string{"1024x1024": "1:1", "1024x1792": "9:16", "1792x1024": "16:9"}[size]; ratio != "" {
			body["aspect_ratio"] = ratio
		}
	}
	for _, name := range []string{"prompt", "image", "mask", "init_image", "style_image", "negative_prompt", "aspect_ratio",
		"output_format", "mode", "style_preset", "select_prompt", "search_prompt"} {
		if value, _ := fields[name].(string); value != "" {
			body[name] = value
		}
	}
	if image := body["image"]; image != nil && strings.Contains(model, "style-transfer") {
		if body["init_image"] == nil {
			body["init_image"] = image
		}
		delete(body, "image")
	}
	for _, name := range []string{"left", "right", "up", "down", "seed", "strength", "creativity", "control_strength",
		"grow_mask", "fidelity", "composition_fidelity", "style_strength", "change_strength"} {
		switch value := fields[name].(type) {
		case float64:
			body[name] = value
		case string:
			number, err := strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
				return nil, errors.New("Invalid Bedrock Stability numeric parameter")
			}
			body[name] = number
		}
	}
	return body, nil
}

type bedrockImageRejection struct{ error }

func bedrockImageResponse(body []byte) ([]byte, error) {
	var response struct {
		Images        []string `json:"images"`
		FinishReasons []string `json:"finish_reasons"`
		Artifacts     []struct {
			Base64 string `json:"base64"`
		} `json:"artifacts"`
	}
	if json.Unmarshal(body, &response) != nil {
		return nil, errors.New("Invalid Bedrock image response")
	}
	for _, reason := range response.FinishReasons {
		if reason != "" {
			return nil, bedrockImageRejection{errors.New(reason)}
		}
	}
	data := make([]map[string]string, 0, len(response.Images)+len(response.Artifacts))
	for _, image := range response.Images {
		if image != "" {
			data = append(data, map[string]string{"b64_json": image})
		}
	}
	for _, artifact := range response.Artifacts {
		if artifact.Base64 != "" {
			data = append(data, map[string]string{"b64_json": artifact.Base64})
		}
	}
	if len(data) == 0 {
		return nil, errors.New("Bedrock returned no images")
	}
	return json.Marshal(map[string]any{"created": time.Now().Unix(), "data": data})
}
