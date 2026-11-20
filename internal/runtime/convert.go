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
		for _, item := range typed {
			messages = append(messages, responsesInputToChat(item))
		}
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
		if role == "assistant" {
			if calls, ok := message["tool_calls"].([]any); ok {
				for _, call := range calls {
					if item := chatToolCallToResponse(call); item != nil {
						prompt = append(prompt, item)
					}
				}
			}
			if content != nil && content != "" {
				prompt = append(prompt, map[string]any{"role": role, "content": content})
			}
			continue
		}
		if role == "tool" {
			prompt = append(prompt, map[string]any{"type": "function_call_output", "call_id": message["tool_call_id"], "output": content})
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
	output := []any{}
	if choices, ok := input["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if message, ok := choice["message"].(map[string]any); ok {
				content, _ = message["content"].(string)
				if calls, ok := message["tool_calls"].([]any); ok {
					for _, call := range calls {
						if item := chatToolCallToResponse(call); item != nil {
							output = append(output, item)
						}
					}
				}
			}
		}
	}
	if content != "" {
		output = append([]any{map[string]any{"id": "runtime-message", "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": content, "annotations": []any{}}}}}, output...)
	}
	return map[string]any{
		"id": input["id"], "object": "response", "created_at": input["created"],
		"status": "completed", "model": alias,
		"output": output,
		"usage":  input["usage"],
	}
}

func responsesToChatResponse(input map[string]any, alias string) map[string]any {
	content := ""
	toolCalls := []any{}
	if output, ok := input["output"].([]any); ok {
		for _, item := range output {
			message, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if message["type"] != "message" {
				if message["type"] == "function_call" {
					toolCalls = append(toolCalls, map[string]any{"id": message["call_id"], "type": "function", "function": map[string]any{"name": message["name"], "arguments": message["arguments"]}})
				}
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
	message := map[string]any{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		message["content"] = nil
	}
	return map[string]any{
		"id": input["id"], "object": "chat.completion", "created": input["created_at"], "model": alias,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": map[bool]string{true: "tool_calls", false: "stop"}[len(toolCalls) > 0]}},
		"usage":   input["usage"],
	}
}

func responsesInputToChat(item any) map[string]any {
	value, ok := item.(map[string]any)
	if !ok {
		return map[string]any{"role": "user", "content": fmt.Sprint(item)}
	}
	if value["type"] == "function_call" {
		return map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": value["call_id"], "type": "function", "function": map[string]any{"name": value["name"], "arguments": value["arguments"]}}}}
	}
	if value["type"] == "function_call_output" {
		return map[string]any{"role": "tool", "tool_call_id": value["call_id"], "content": value["output"]}
	}
	return value
}

func chatToolCallToResponse(value any) map[string]any {
	call, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	function, _ := call["function"].(map[string]any)
	return map[string]any{"type": "function_call", "call_id": call["id"], "name": function["name"], "arguments": function["arguments"], "status": "completed"}
}

func clone(value map[string]any) map[string]any {
	encoded, _ := json.Marshal(value)
	var result map[string]any
	_ = json.Unmarshal(encoded, &result)
	return result
}
