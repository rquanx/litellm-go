package runtime

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"strconv"
	"strings"
	"time"
)

func bedrockCanvasDeployment(candidate Candidate) bool {
	return candidate.Provider == "bedrock" && strings.Contains(candidate.ModelID, "amazon.nova-canvas") &&
		(candidate.Endpoint == "image-generation" || candidate.Endpoint == "image-edit")
}

func bedrockCanvasPayload(input requestPayload, edit bool) (map[string]any, error) {
	fields := clone(input.fields)
	var image, mask string
	for _, part := range input.form {
		switch part.name {
		case "image", "image[]":
			if image != "" || len(part.body) == 0 {
				return nil, errors.New("Nova Canvas requires one nonempty image")
			}
			image = base64.StdEncoding.EncodeToString(part.body)
		case "mask":
			if len(part.body) == 0 || mask != "" {
				return nil, errors.New("Nova Canvas accepts one nonempty mask")
			}
			mask = base64.StdEncoding.EncodeToString(part.body)
		case "n", "width", "height", "seed", "numberOfImages", "cfgScale", "similarityStrength":
			number, err := strconv.ParseFloat(string(part.body), 64)
			if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
				return nil, errors.New("Invalid Nova Canvas numeric parameter")
			}
			fields[part.name] = number
		case "imageGenerationConfig":
			var config map[string]any
			if err := json.Unmarshal(part.body, &config); err != nil {
				return nil, errors.New("Invalid Nova Canvas image generation configuration")
			}
			fields[part.name] = config
		case "prompt", "size", "quality", "taskType", "maskPrompt", "negativeText", "outPaintingMode":
			fields[part.name] = string(part.body)
		}
	}
	suppliedConfig, _ := fields["imageGenerationConfig"].(map[string]any)
	config := make(map[string]any, len(suppliedConfig)+6)
	maps.Copy(config, suppliedConfig)
	if size, _ := fields["size"].(string); size != "" {
		width, height, _ := strings.Cut(size, "x")
		w, errW := strconv.Atoi(width)
		h, errH := strconv.Atoi(height)
		if errW != nil || errH != nil || w < 1 || h < 1 {
			return nil, errors.New("Nova Canvas size must contain a positive width and height")
		}
		config["width"], config["height"] = w, h
	}
	if count := fields["n"]; count != nil {
		config["numberOfImages"] = count
	}
	for _, name := range []string{"width", "height", "numberOfImages", "quality", "cfgScale", "seed"} {
		if value := fields[name]; value != nil {
			config[name] = value
		}
	}
	if config["quality"] == "hd" {
		config["quality"] = "premium"
	}
	text, _ := fields["prompt"].(string)
	task, _ := fields["taskType"].(string)
	if edit {
		if image == "" {
			image, _ = fields["image"].(string)
		}
		if mask == "" {
			mask, _ = fields["mask"].(string)
		}
		payload, err := bedrockCanvasEdit(fields, task, text, image, mask)
		if err == nil && len(config) > 0 && payload["taskType"] != "BACKGROUND_REMOVAL" {
			payload["imageGenerationConfig"] = config
		}
		return payload, err
	}
	if text == "" {
		return nil, errors.New("Nova Canvas generation requires a prompt")
	}
	if task == "" {
		task = "TEXT_IMAGE"
	}
	name := map[string]string{"TEXT_IMAGE": "textToImageParams", "COLOR_GUIDED_GENERATION": "colorGuidedGenerationParams", "INPAINTING": "inpaintingParams"}[task]
	if name == "" {
		return nil, errors.New("Unsupported Nova Canvas generation task")
	}
	params := map[string]any{"text": text}
	supplied, _ := fields[name].(map[string]any)
	for key, value := range supplied {
		params[key] = value
	}
	return map[string]any{"taskType": task, name: params, "imageGenerationConfig": config}, nil
}

func bedrockCanvasEdit(fields map[string]any, task, text, image, mask string) (map[string]any, error) {
	if image == "" {
		return nil, errors.New("Nova Canvas edit requires an image")
	}
	maskPrompt, _ := fields["maskPrompt"].(string)
	if task == "" {
		task = "IMAGE_VARIATION"
		if mask != "" || maskPrompt != "" {
			task = "INPAINTING"
		}
	}
	if task == "BACKGROUND_REMOVAL" {
		return map[string]any{"taskType": task, "backgroundRemovalParams": map[string]string{"image": image}}, nil
	}
	name := map[string]string{"IMAGE_VARIATION": "imageVariationParams", "INPAINTING": "inPaintingParams", "OUTPAINTING": "outPaintingParams"}[task]
	if name == "" {
		return nil, errors.New("Unsupported Nova Canvas edit task")
	}
	params := make(map[string]any)
	if negative := fields["negativeText"]; negative != nil {
		params["negativeText"] = negative
	}
	if task == "IMAGE_VARIATION" {
		params["images"] = []string{image}
		if strength := fields["similarityStrength"]; strength != nil {
			params["similarityStrength"] = strength
		}
	} else {
		params["image"] = image
		switch {
		case maskPrompt != "":
			params["maskPrompt"] = maskPrompt
		case mask != "":
			params["maskImage"] = mask
		default:
			return nil, errors.New("Nova Canvas painting requires a mask or a mask prompt")
		}
		if explicit, _ := fields["taskType"].(string); explicit != "" && text == "" {
			return nil, errors.New("Nova Canvas painting requires a prompt")
		}
		if mode := fields["outPaintingMode"]; task == "OUTPAINTING" && mode != nil {
			params["outPaintingMode"] = mode
		}
	}
	if text == "" {
		text = " "
	}
	params["text"] = text
	return map[string]any{"taskType": task, name: params}, nil
}

func bedrockCanvasResponse(body []byte) ([]byte, error) {
	var response struct {
		Images []string `json:"images"`
	}
	if json.Unmarshal(body, &response) != nil || len(response.Images) == 0 {
		return nil, errors.New("Nova Canvas returned no images")
	}
	data := make([]map[string]string, 0, len(response.Images))
	for _, image := range response.Images {
		if image != "" {
			data = append(data, map[string]string{"b64_json": image})
		}
	}
	if len(data) == 0 {
		return nil, errors.New("Nova Canvas returned no images")
	}
	return json.Marshal(map[string]any{"created": time.Now().Unix(), "data": data})
}
