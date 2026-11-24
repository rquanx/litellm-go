package runtime

import "strings"

func applyChatExtraBody(payload map[string]any) {
	extra, ok := payload["extra_body"].(map[string]any)
	if !ok {
		return
	}
	delete(payload, "extra_body")
	for key, value := range extra {
		switch key {
		case "model", "stream", "drop_params", "allowed_openai_params", "additional_drop_params":
			continue
		}
		payload[key] = value
	}
}

func applyProviderChatSettings(payload map[string]any, provider Candidate) {
	for from, to := range provider.ParamMappings {
		if from != to {
			if value, ok := payload[from]; ok {
				payload[to] = value
				delete(payload, from)
			}
		}
	}
	if !provider.TextOnly {
		return
	}
	messages, _ := payload["messages"].([]any)
	for _, entry := range messages {
		message, _ := entry.(map[string]any)
		parts, ok := message["content"].([]any)
		if !ok {
			continue
		}
		var content strings.Builder
		for _, part := range parts {
			switch value := part.(type) {
			case string:
				content.WriteString(value)
			case map[string]any:
				if value["type"] == "text" {
					if line, ok := value["text"].(string); ok {
						content.WriteString(line)
					}
				}
			}
		}
		message["content"] = content.String()
	}
}
