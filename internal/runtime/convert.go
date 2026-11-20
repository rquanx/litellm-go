package runtime

import (
	"encoding/json"
	"fmt"
)

func responsesToChat(input map[string]any) map[string]any {
	result := clone(input)
	delete(result, "instructions")
	value := result["input"]
	delete(result, "input")
	messages := []any{}
	if instructions, ok := input["instructions"].(string); ok && instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	switch typed := value.(type) {
	case string:
		messages = append(messages, map[string]any{"role": "user", "content": typed})
	case []any:
		messages = append(messages, typed...)
	default:
		messages = append(messages, map[string]any{"role": "user", "content": fmt.Sprint(value)})
	}
	result["messages"] = messages
	if value, ok := result["max_output_tokens"]; ok {
		result["max_tokens"] = value
		delete(result, "max_output_tokens")
	}
	if text, ok := input["text"].(map[string]any); ok {
		if format, ok := text["format"].(map[string]any); ok {
			result["response_format"] = format
		}
	}
	delete(result, "text")
	return result
}

func chatRequestToResponses(input map[string]any) map[string]any {
	result := clone(input)
	delete(result, "messages")
	messages, _ := input["messages"].([]any)
	var instructions string
	var prompt []any
	for _, item := range messages {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		content := message["content"]
		if role == "system" {
			if value, ok := content.(string); ok {
				if instructions != "" {
					instructions += "\n"
				}
				instructions += value
			}
			continue
		}
		prompt = append(prompt, map[string]any{"role": role, "content": content})
	}
	result["input"] = prompt
	if instructions != "" {
		result["instructions"] = instructions
	}
	if value, ok := result["max_tokens"]; ok {
		result["max_output_tokens"] = value
		delete(result, "max_tokens")
	}
	if value, ok := result["max_completion_tokens"]; ok {
		result["max_output_tokens"] = value
		delete(result, "max_completion_tokens")
	}
	if value, ok := result["response_format"]; ok {
		result["text"] = map[string]any{"format": value}
		delete(result, "response_format")
	}
	delete(result, "stream_options")
	return result
}

func chatToResponses(input map[string]any, alias string) map[string]any {
	content := ""
	if choices, ok := input["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if message, ok := choice["message"].(map[string]any); ok {
				content, _ = message["content"].(string)
			}
		}
	}
	return map[string]any{
		"id": input["id"], "object": "response", "created_at": input["created"],
		"status": "completed", "model": alias,
		"output": []any{map[string]any{"id": "runtime-message", "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": content, "annotations": []any{}}}}},
		"usage": input["usage"],
	}
}

func responsesToChatResponse(input map[string]any, alias string) map[string]any {
	content := ""
	if output, ok := input["output"].([]any); ok {
		for _, item := range output {
			message, ok := item.(map[string]any)
			if !ok || message["type"] != "message" {
				continue
			}
			parts, _ := message["content"].([]any)
			for _, part := range parts {
				value, ok := part.(map[string]any)
				if ok && value["type"] == "output_text" {
					content, _ = value["text"].(string)
					break
				}
			}
			if content != "" {
				break
			}
		}
	}
	return map[string]any{
		"id": input["id"], "object": "chat.completion", "created": input["created_at"], "model": alias,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		"usage":   input["usage"],
	}
}

func clone(value map[string]any) map[string]any {
	encoded, _ := json.Marshal(value)
	var result map[string]any
	_ = json.Unmarshal(encoded, &result)
	return result
}
