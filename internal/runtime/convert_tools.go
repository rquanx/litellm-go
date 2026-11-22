package runtime

import "maps"

func convertFunctionTools(request map[string]any, toChat bool) {
	if tools, ok := request["tools"].([]any); ok {
		converted := make([]any, 0, len(tools))
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok || tool["type"] != "function" {
				converted = append(converted, raw)
				continue
			}
			converted = append(converted, convertFunctionTool(tool, toChat))
		}
		request["tools"] = converted
	}
	if choice, ok := request["tool_choice"].(map[string]any); ok {
		request["tool_choice"] = convertToolChoice(choice, toChat)
	}
}

func convertFunctionTool(tool map[string]any, toChat bool) map[string]any {
	fields := tool
	if nested, ok := tool["function"].(map[string]any); ok {
		fields = nested
	}
	parameters, _ := fields["parameters"].(map[string]any)
	schema := map[string]any{"type": "object"}
	maps.Copy(schema, parameters)
	name, _ := fields["name"].(string)
	description, _ := fields["description"].(string)
	strict, _ := fields["strict"].(bool)
	function := map[string]any{"name": name, "description": description, "parameters": schema, "strict": strict}
	result := map[string]any{"type": "function"}
	if toChat {
		result["function"] = function
	} else {
		maps.Copy(result, function)
	}
	for _, key := range []string{"cache_control", "defer_loading", "allowed_callers", "input_examples"} {
		if tool[key] != nil {
			result[key] = tool[key]
		}
	}
	return result
}

func convertToolChoice(choice map[string]any, toChat bool) any {
	kind, _ := choice["type"].(string)
	if kind == "function" {
		name, _ := choice["name"].(string)
		if name == "" {
			function, _ := choice["function"].(map[string]any)
			name, _ = function["name"].(string)
		}
		if name != "" {
			if toChat {
				return map[string]any{"type": "function", "function": map[string]any{"name": name}}
			}
			return map[string]any{"type": "function", "name": name}
		}
	}
	if toChat {
		switch kind {
		case "auto", "none":
			return kind
		case "required", "tool", "any", "function":
			return "required"
		}
	}
	return choice
}
