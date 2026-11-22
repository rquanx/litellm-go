package runtime

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
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
			message := responsesInputToChat(item)
			if calls, ok := message["tool_calls"].([]any); ok && len(messages) > 0 {
				previous, _ := messages[len(messages)-1].(map[string]any)
				if previous["role"] == "assistant" {
					existing, _ := previous["tool_calls"].([]any)
					previous["tool_calls"] = append(existing, calls...)
					continue
				}
			}
			messages = append(messages, message)
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
			switch format["type"] {
			case "json_schema":
				result["response_format"] = map[string]any{"type": "json_schema", "json_schema": schemaOptions(format)}
			case "json_object":
				result["response_format"] = map[string]any{"type": "json_object"}
			}
		}
	}
	delete(result, "text")
	convertFunctionTools(result, true)
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
			if content != nil && content != "" {
				prompt = append(prompt, map[string]any{"role": role, "content": convertContent(content, role, false)})
			}
			if calls, ok := message["tool_calls"].([]any); ok {
				for _, call := range calls {
					if item := chatToolCallToResponse(call); item != nil {
						prompt = append(prompt, item)
					}
				}
			}
			continue
		}
		if role == "tool" {
			prompt = append(prompt, map[string]any{"type": "function_call_output", "call_id": message["tool_call_id"], "output": content})
			continue
		}
		prompt = append(prompt, map[string]any{"role": role, "content": convertContent(content, role, false)})
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
	if format, ok := input["response_format"].(map[string]any); ok {
		switch format["type"] {
		case "json_schema":
			schema, _ := format["json_schema"].(map[string]any)
			options := schemaOptions(schema)
			options["type"] = "json_schema"
			result["text"] = map[string]any{"format": options}
		case "json_object", "text":
			result["text"] = map[string]any{"format": map[string]any{"type": format["type"]}}
		}
	}
	delete(result, "response_format")
	delete(result, "stream_options")
	convertFunctionTools(result, false)
	return result
}

func schemaOptions(format map[string]any) map[string]any {
	options := map[string]any{"name": "response_schema", "schema": map[string]any{}, "strict": false}
	for key := range options {
		if value, exists := format[key]; exists {
			options[key] = value
		}
	}
	return options
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
		"usage":  convertTokenUsage(input["usage"], true),
	}
}

func responsesToChatResponse(input map[string]any, alias string) map[string]any {
	var content strings.Builder
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
					text, _ := value["text"].(string)
					content.WriteString(text)
				}
			}
		}
	}
	message := map[string]any{"role": "assistant", "content": content.String()}
	finish := "stop"
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		finish = "tool_calls"
		if content.Len() == 0 {
			message["content"] = nil
		}
	}
	return map[string]any{
		"id": input["id"], "object": "chat.completion", "created": input["created_at"], "model": alias,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage":   convertTokenUsage(input["usage"], false),
	}
}

func convertTokenUsage(value any, toResponses bool) any {
	usage, ok := value.(map[string]any)
	if !ok {
		return value
	}
	result := make(map[string]any, len(usage))
	for key, value := range usage {
		result[key] = value
	}
	fields := [][2]string{
		{"prompt_tokens", "input_tokens"},
		{"completion_tokens", "output_tokens"},
		{"prompt_tokens_details", "input_tokens_details"},
		{"completion_tokens_details", "output_tokens_details"},
	}
	for _, field := range fields {
		from, to := field[0], field[1]
		if !toResponses {
			from, to = to, from
		}
		if value, present := result[from]; present {
			result[to] = value
			delete(result, from)
		}
	}
	return result
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
	message := maps.Clone(value)
	if content, present := value["content"]; present {
		role, _ := value["role"].(string)
		message["content"] = convertContent(content, role, true)
	}
	if message["type"] == "message" {
		delete(message, "type")
		delete(message, "id")
		delete(message, "status")
	}
	return message
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
