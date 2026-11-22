package runtime

import (
	"encoding/json"
	"maps"
	"strings"
	"time"
)

func isTextEndpoint(endpoint string) bool {
	return endpoint == "chat-completions" || endpoint == "responses"
}

func chatToAnthropicRequest(chat map[string]any) map[string]any {
	native := map[string]any{"model": chat["model"], "stream": chat["stream"], "max_tokens": 4096}
	for _, key := range []string{"temperature", "top_p", "metadata", "thinking"} {
		if value, present := chat[key]; present {
			native[key] = value
		}
	}
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if value, present := chat[key]; present {
			native["max_tokens"] = value
		}
	}
	switch stop := chat["stop"].(type) {
	case string:
		native["stop_sequences"] = []string{stop}
	case []any:
		native["stop_sequences"] = stop
	}
	native["messages"], native["system"] = anthropicMessages(chat["messages"])
	if len(native["system"].([]any)) == 0 {
		delete(native, "system")
	}
	if tools := anthropicTools(chat["tools"]); len(tools) != 0 {
		native["tools"] = tools
		if choice := anthropicToolChoice(chat); choice != nil {
			native["tool_choice"] = choice
		}
	}
	return native
}

func anthropicMessages(raw any) ([]any, []any) {
	messages, _ := raw.([]any)
	result := make([]any, 0, len(messages))
	system := []any{}
	for _, value := range messages {
		message, _ := value.(map[string]any)
		role, _ := message["role"].(string)
		content := anthropicContent(message["content"])
		switch role {
		case "system", "developer":
			system = append(system, content...)
			continue
		case "tool":
			role = "user"
			content = []any{map[string]any{
				"type": "tool_result", "tool_use_id": message["tool_call_id"], "content": content,
			}}
		case "assistant":
			calls, _ := message["tool_calls"].([]any)
			for _, rawCall := range calls {
				call, _ := rawCall.(map[string]any)
				function, _ := call["function"].(map[string]any)
				arguments, _ := function["arguments"].(string)
				input := map[string]any{}
				if arguments != "" {
					_ = json.Unmarshal([]byte(arguments), &input)
				}
				content = append(content, map[string]any{
					"type": "tool_use", "id": call["id"], "name": function["name"], "input": input,
				})
			}
		case "user":
		default:
			continue
		}
		if len(result) != 0 {
			previous := result[len(result)-1].(map[string]any)
			if previous["role"] == role {
				previous["content"] = append(previous["content"].([]any), content...)
				continue
			}
		}
		result = append(result, map[string]any{"role": role, "content": content})
	}
	return result, system
}

func anthropicContent(raw any) []any {
	result := []any{}
	if text, ok := raw.(string); ok && text != "" {
		return append(result, map[string]any{"type": "text", "text": text})
	}
	parts, _ := raw.([]any)
	for _, part := range parts {
		block, _ := part.(map[string]any)
		switch block["type"] {
		case "text":
			result = append(result, block)
		case "image_url":
			image, _ := block["image_url"].(map[string]any)
			url, _ := image["url"].(string)
			source := map[string]any{"type": "url", "url": url}
			if rest, ok := strings.CutPrefix(url, "data:"); ok {
				if media, data, ok := strings.Cut(rest, ";base64,"); ok {
					source = map[string]any{"type": "base64", "media_type": media, "data": data}
				}
			}
			if url != "" {
				result = append(result, map[string]any{"type": "image", "source": source})
			}
		}
	}
	return result
}

func anthropicTools(raw any) []any {
	tools, _ := raw.([]any)
	result := make([]any, 0, len(tools))
	for _, item := range tools {
		tool, _ := item.(map[string]any)
		function, _ := tool["function"].(map[string]any)
		if tool["type"] != "function" || function["name"] == nil {
			continue
		}
		parameters, _ := function["parameters"].(map[string]any)
		input := make(map[string]any, len(parameters)+1)
		maps.Copy(input, parameters)
		input["type"] = "object"
		mapped := map[string]any{"name": function["name"], "input_schema": input}
		if value, present := function["description"]; present {
			mapped["description"] = value
		}
		if value, present := function["strict"]; present {
			mapped["strict"] = value
		}
		result = append(result, mapped)
	}
	return result
}

func anthropicToolChoice(chat map[string]any) map[string]any {
	var choice map[string]any
	switch value := chat["tool_choice"].(type) {
	case string:
		switch value {
		case "none":
			return map[string]any{"type": "none"}
		case "required":
			choice = map[string]any{"type": "any"}
		case "auto":
			choice = map[string]any{"type": "auto"}
		}
	case map[string]any:
		function, _ := value["function"].(map[string]any)
		if value["type"] == "function" && function["name"] != nil {
			choice = map[string]any{"type": "tool", "name": function["name"]}
		}
	}
	if parallel, ok := chat["parallel_tool_calls"].(bool); ok && !parallel {
		if choice == nil {
			choice = map[string]any{"type": "auto"}
		}
		choice["disable_parallel_tool_use"] = true
	}
	return choice
}

func anthropicToChatResponse(native map[string]any, alias string) map[string]any {
	var text, thinking strings.Builder
	calls := []any{}
	blocks, _ := native["content"].([]any)
	for _, block := range blocks {
		value, _ := block.(map[string]any)
		switch value["type"] {
		case "text":
			part, _ := value["text"].(string)
			text.WriteString(part)
		case "thinking":
			part, _ := value["thinking"].(string)
			thinking.WriteString(part)
		case "tool_use":
			arguments, _ := json.Marshal(value["input"])
			calls = append(calls, map[string]any{"id": value["id"], "type": "function",
				"function": map[string]any{"name": value["name"], "arguments": string(arguments)}})
		}
	}
	message := map[string]any{"role": "assistant", "content": text.String()}
	if len(calls) != 0 {
		message["tool_calls"] = calls
		if text.Len() == 0 {
			message["content"] = nil
		}
	}
	if thinking.Len() != 0 {
		message["reasoning_content"] = thinking.String()
	}
	rawUsage, _ := native["usage"].(map[string]any)
	return map[string]any{
		"id": native["id"], "object": "chat.completion", "created": time.Now().Unix(), "model": alias,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": anthropicFinish(native["stop_reason"])}},
		"usage": anthropicUsage(rawUsage),
	}
}

func anthropicFinish(value any) string {
	switch value {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use", "pause_turn":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

func anthropicUsage(raw map[string]any) any {
	if raw == nil {
		return nil
	}
	input, _ := raw["input_tokens"].(float64)
	output, _ := raw["output_tokens"].(float64)
	read, _ := raw["cache_read_input_tokens"].(float64)
	created, _ := raw["cache_creation_input_tokens"].(float64)
	prompt := input + read + created
	usage := map[string]any{"prompt_tokens": prompt, "completion_tokens": output, "total_tokens": prompt + output}
	if read != 0 || created != 0 {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": read, "cache_creation_tokens": created}
	}
	return usage
}
