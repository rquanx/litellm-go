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

func clone(value map[string]any) map[string]any {
	encoded, _ := json.Marshal(value)
	var result map[string]any
	_ = json.Unmarshal(encoded, &result)
	return result
}
