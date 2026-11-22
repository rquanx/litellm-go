package runtime

import "maps"

func convertContent(content any, role string, toChat bool) any {
	parts, ok := content.([]any)
	if !ok {
		return content
	}
	result := make([]any, 0, len(parts))
	for _, part := range parts {
		block, ok := part.(map[string]any)
		if !ok {
			result = append(result, part)
			continue
		}
		result = append(result, convertContentBlock(block, role, toChat))
	}
	return result
}

func convertContentBlock(block map[string]any, role string, toChat bool) map[string]any {
	kind, _ := block["type"].(string)
	if toChat {
		switch kind {
		case "input_text", "output_text":
			text := maps.Clone(block)
			text["type"] = "text"
			delete(text, "annotations")
			return text
		case "input_image":
			url, _ := block["image_url"].(string)
			if url == "" {
				return block
			}
			image := maps.Clone(block)
			image["type"] = "image_url"
			image["image_url"] = map[string]any{"url": url, "detail": imageDetail(block)}
			delete(image, "detail")
			return image
		}
		return block
	}
	switch kind {
	case "text":
		text := maps.Clone(block)
		text["type"] = "input_text"
		if role == "assistant" {
			text["type"] = "output_text"
		}
		return text
	case "image_url":
		url, _ := block["image_url"].(string)
		detail := "auto"
		if image, ok := block["image_url"].(map[string]any); ok {
			url, _ = image["url"].(string)
			detail = imageDetail(image)
		}
		if url != "" {
			return map[string]any{"type": "input_image", "image_url": url, "detail": detail}
		}
	}
	return block
}

func imageDetail(image map[string]any) string {
	detail, _ := image["detail"].(string)
	if detail == "" {
		return "auto"
	}
	return detail
}
