package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
)

var bedrockToolNameChars = regexp.MustCompile(`[^a-zA-Z0-9_]`)

func (s *Server) countBedrockInput(ctx context.Context, request *http.Request, candidate Candidate, fields map[string]any) (int, []byte, http.Header, bool, error) {
	status, body, headers, retryable, err := s.callProviderJSON(ctx, request, "bedrock-count-tokens", candidate, requestPayload{fields: fields})
	if err != nil || status/100 != 2 {
		return status, body, headers, retryable, err
	}
	var native struct {
		Count *int `json:"inputTokens"`
	}
	if json.Unmarshal(body, &native) != nil || native.Count == nil || *native.Count < 0 {
		return http.StatusBadGateway, nil, headers, true, errors.New("Invalid Bedrock token count")
	}
	body, err = json.Marshal(map[string]int{"input_tokens": *native.Count})
	headers = http.Header{"Content-Type": {"application/json"}}
	return status, body, headers, false, err
}

func bedrockTokenCountPayload(messages []map[string]any, system any, tools []any) map[string]any {
	conversation := make([]map[string]any, 0, len(messages))
	systemBlocks := bedrockBlocks(anthropicContent(system))
	for _, message := range messages {
		role, _ := message["role"].(string)
		if role == "system" || role == "developer" {
			systemBlocks = append(systemBlocks, bedrockBlocks(anthropicContent(message["content"]))...)
			continue
		}
		content := tokenCountBedrockContent(message["content"])
		if calls, ok := message["tool_calls"].([]any); role == "tool" || (role == "assistant" && ok && len(calls) != 0) {
			converted, _ := anthropicMessages([]any{message})
			for _, entry := range converted {
				current := entry.(map[string]any)
				role, _ = current["role"].(string)
				blocks, _ := current["content"].([]any)
				content = bedrockBlocks(blocks)
			}
		}
		conversation = append(conversation, map[string]any{"role": role, "content": content})
	}
	converse := map[string]any{"messages": conversation}
	if len(systemBlocks) != 0 {
		converse["system"] = systemBlocks
	}
	specs := make([]any, 0, len(tools))
	for _, item := range tools {
		tool, _ := item.(map[string]any)
		name, _ := tool["name"].(string)
		description, _ := tool["description"].(string)
		schema, _ := tool["input_schema"].(map[string]any)
		if tool["type"] == "function" {
			function, _ := tool["function"].(map[string]any)
			name, _ = function["name"].(string)
			description, _ = function["description"].(string)
			schema, _ = function["parameters"].(map[string]any)
		}
		name = bedrockToolNameChars.ReplaceAllString(name, "_")
		if name == "" {
			continue
		}
		if name[0] == '_' || (name[0] >= '0' && name[0] <= '9') {
			name = "t_" + name
		}
		name = name[:min(len(name), 64)]
		if description == "" {
			description = name
		}
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		specs = append(specs, map[string]any{"toolSpec": map[string]any{
			"name": name, "description": description, "inputSchema": map[string]any{"json": schema},
		}})
	}
	if len(specs) != 0 {
		converse["toolConfig"] = map[string]any{"tools": specs}
	}
	return map[string]any{"input": map[string]any{"converse": converse}}
}

func tokenCountBedrockContent(content any) []any {
	if text, ok := content.(string); ok {
		return []any{map[string]any{"text": text}}
	}
	parts, _ := content.([]any)
	result := make([]any, 0, len(parts))
	for _, value := range parts {
		part, _ := value.(map[string]any)
		switch part["type"] {
		case nil:
			if len(part) > 0 {
				result = append(result, part)
			}
		case "image_url":
			result = append(result, bedrockBlocks(anthropicContent([]any{part}))...)
		default:
			result = append(result, bedrockBlocks([]any{part})...)
		}
	}
	return result
}
