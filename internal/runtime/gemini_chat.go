package runtime

import (
	"encoding/json"
	"strings"
	"time"
)

func chatToGemini(chat map[string]any) map[string]any {
	request := map[string]any{}
	config := map[string]any{}
	for key, target := range map[string]string{
		"temperature": "temperature", "top_p": "topP", "max_tokens": "maxOutputTokens", "n": "candidateCount",
	} {
		if value, found := chat[key]; found {
			config[target] = value
		}
	}
	if value, found := chat["max_completion_tokens"]; found {
		config["maxOutputTokens"] = value
	}
	switch stop := chat["stop"].(type) {
	case string:
		config["stopSequences"] = []string{stop}
	case []any:
		config["stopSequences"] = stop
	}
	format, _ := chat["response_format"].(map[string]any)
	switch format["type"] {
	case "json_object":
		config["responseMimeType"] = "application/json"
	case "json_schema":
		schema, _ := format["json_schema"].(map[string]any)
		config["responseMimeType"] = "application/json"
		if value, found := schema["schema"]; found {
			config["responseJsonSchema"] = value
		}
	}
	if len(config) != 0 {
		request["generationConfig"] = config
	}
	messages, _ := chat["messages"].([]any)
	system, contents := []any{}, []any{}
	callNames := make(map[string]string)
	for _, entry := range messages {
		message, _ := entry.(map[string]any)
		role, _ := message["role"].(string)
		parts := geminiParts(message["content"])
		switch role {
		case "system", "developer":
			system = append(system, parts...)
			continue
		case "assistant":
			role = "model"
			calls, _ := message["tool_calls"].([]any)
			for _, entry := range calls {
				call, _ := entry.(map[string]any)
				fn, _ := call["function"].(map[string]any)
				id, _ := call["id"].(string)
				name, _ := fn["name"].(string)
				callNames[id] = name
				arguments, _ := fn["arguments"].(string)
				args := map[string]any{}
				_ = json.Unmarshal([]byte(arguments), &args)
				parts = append(parts, map[string]any{"functionCall": map[string]any{"name": name, "args": args}})
			}
		case "tool":
			role = "user"
			id, _ := message["tool_call_id"].(string)
			content, _ := message["content"].(string)
			result := map[string]any{"result": content}
			var parsed map[string]any
			if json.Unmarshal([]byte(content), &parsed) == nil && parsed != nil {
				result = parsed
			}
			parts = []any{map[string]any{"functionResponse": map[string]any{"name": callNames[id], "response": result}}}
		case "user":
		default:
			continue
		}
		if len(parts) != 0 {
			contents = append(contents, map[string]any{"role": role, "parts": parts})
		}
	}
	request["contents"] = contents
	if len(system) != 0 {
		request["systemInstruction"] = map[string]any{"parts": system}
	}
	tools, _ := chat["tools"].([]any)
	declarations := []any{}
	for _, item := range tools {
		tool, _ := item.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		if tool["type"] == "function" && fn["name"] != nil {
			declaration := map[string]any{"name": fn["name"]}
			if value, found := fn["description"]; found {
				declaration["description"] = value
			}
			if value, found := fn["parameters"]; found {
				declaration["parametersJsonSchema"] = value
			}
			declarations = append(declarations, declaration)
		}
	}
	if len(declarations) != 0 {
		request["tools"] = []any{map[string]any{"functionDeclarations": declarations}}
		if choice := geminiToolChoice(chat["tool_choice"]); choice != nil {
			request["toolConfig"] = map[string]any{"functionCallingConfig": choice}
		}
	}
	return request
}

func geminiToolChoice(input any) map[string]any {
	switch choice := input.(type) {
	case string:
		switch choice {
		case "none":
			return map[string]any{"mode": "NONE"}
		case "auto":
			return map[string]any{"mode": "AUTO"}
		case "required":
			return map[string]any{"mode": "ANY"}
		}
	case map[string]any:
		function, _ := choice["function"].(map[string]any)
		if name, ok := function["name"].(string); ok && name != "" {
			return map[string]any{"mode": "ANY", "allowedFunctionNames": []string{name}}
		}
	}
	return nil
}

func geminiParts(input any) []any {
	result := []any{}
	if text, ok := input.(string); ok && text != "" {
		return append(result, map[string]any{"text": text})
	}
	parts, _ := input.([]any)
	for _, item := range parts {
		part, _ := item.(map[string]any)
		switch part["type"] {
		case "text":
			result = append(result, map[string]any{"text": part["text"]})
		case "image_url":
			image, _ := part["image_url"].(map[string]any)
			address, _ := image["url"].(string)
			if data, ok := strings.CutPrefix(address, "data:"); ok {
				if media, body, ok := strings.Cut(data, ";base64,"); ok {
					result = append(result, map[string]any{"inlineData": map[string]any{"mimeType": media, "data": body}})
					continue
				}
			}
			if address != "" {
				result = append(result, map[string]any{"fileData": map[string]any{"fileUri": address}})
			}
		}
	}
	return result
}

func geminiChatResponse(native map[string]any, alias string) map[string]any {
	candidates, _ := native["candidates"].([]any)
	choices := make([]any, 0, len(candidates))
	for index, value := range candidates {
		candidate, _ := value.(map[string]any)
		message := geminiCandidateChatMessage(candidate)
		message["role"] = "assistant"
		choices = append(choices, map[string]any{"index": index, "message": message,
			"finish_reason": geminiFinish(candidate["finishReason"], message["tool_calls"] != nil)})
	}
	id, _ := native["responseId"].(string)
	if id == "" {
		id = "chatcmpl_" + newUUID()
	}
	return map[string]any{"id": id, "object": "chat.completion", "created": time.Now().Unix(),
		"model": alias, "choices": choices, "usage": convertTokenUsage(geminiUsage(native), false)}
}

func geminiCandidateChatMessage(candidate map[string]any) map[string]any {
	content, _ := candidate["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	message := map[string]any{}
	var text, reasoning strings.Builder
	calls := []any{}
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		if value, ok := part["text"].(string); ok {
			if inputBool(part, "thought") {
				reasoning.WriteString(value)
			} else {
				text.WriteString(value)
			}
		}
		if call, ok := part["functionCall"].(map[string]any); ok {
			arguments, _ := json.Marshal(call["args"])
			id, _ := call["id"].(string)
			if id == "" {
				id = "call_" + newUUID()
			}
			calls = append(calls, map[string]any{"id": id, "type": "function",
				"function": map[string]any{"name": call["name"], "arguments": string(arguments)}})
		}
	}
	if text.Len() != 0 {
		message["content"] = text.String()
	}
	if reasoning.Len() != 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(calls) != 0 {
		message["tool_calls"] = calls
	}
	return message
}

func geminiFinish(reason any, tools bool) string {
	if tools {
		return "tool_calls"
	}
	switch reason {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	}
	return "stop"
}
