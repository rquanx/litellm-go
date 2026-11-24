package runtime

import "maps"

func applyRouterDefaults(fields, defaults, deployment map[string]any, endpoint string) map[string]any {
	if len(defaults) == 0 {
		return fields
	}
	merged := applyDeploymentDefaults(fields, defaults, endpoint)
	if endpoint == "chat-completions" {
		// Router tools are a default. Configured deployment tools take precedence
		// and are combined with tools supplied on the request.
		deploymentTools, _ := deployment["tools"].([]any)
		if _, requested := fields["tools"]; requested {
			merged["tools"] = fields["tools"]
		} else if len(deploymentTools) > 0 {
			delete(merged, "tools")
		}
	}
	if fallback, ok := defaults["metadata"].(map[string]any); ok && (endpoint == "chat-completions" || endpoint == "responses" || endpoint == "anthropic-messages") {
		key := "metadata"
		if _, exists := fields["litellm_metadata"]; exists || endpoint == "responses" {
			key = "litellm_metadata"
			if _, exists := fields["metadata"]; !exists {
				delete(merged, "metadata")
			}
		}
		current, _ := fields[key].(map[string]any)
		metadata := make(map[string]any, len(current)+len(fallback))
		maps.Copy(metadata, current)
		maps.Copy(metadata, fallback)
		merged[key] = metadata
	}
	return merged
}


func applyDeploymentDefaults(fields, defaults map[string]any, endpoint string) map[string]any {
	if len(defaults) == 0 || !isConversationEndpoint(endpoint) {
		return fields
	}
	merged := maps.Clone(defaults)
	switch endpoint {
	case "chat-completions", "anthropic-messages":
		delete(merged, "max_output_tokens")
		delete(merged, "text")
		delete(merged, "reasoning")
	case "responses":
		for _, name := range []string{"max_tokens", "max_completion_tokens", "response_format", "reasoning_effort",
			"n", "seed", "stop", "frequency_penalty", "presence_penalty", "logit_bias", "logprobs", "top_k", "thinking", "verbosity"} {
			delete(merged, name)
		}
	default:
		return fields
	}
	maps.Copy(merged, fields)
	tools, _ := defaults["tools"].([]any)
	requestTools, _ := fields["tools"].([]any)
	if len(tools) != 0 && endpoint == "chat-completions" {
		merged["tools"] = append(append([]any{}, tools...), requestTools...)
	}
	return merged
}
