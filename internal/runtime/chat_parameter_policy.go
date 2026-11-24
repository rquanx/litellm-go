package runtime

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// LiteLLM v1.85.0 validates the standard optional parameters. Provider-specific
// extensions are independent of the OpenAI parameter catalog.
var optionalChatParams = strings.Fields(`
	functions function_call temperature top_p n stop max_tokens max_completion_tokens
	modalities prediction audio presence_penalty frequency_penalty logit_bias
	response_format seed tools tool_choice logprobs top_logprobs extra_headers
	parallel_tool_calls reasoning_effort verbosity thinking web_search_options
	service_tier safety_identifier prompt_cache_key prompt_cache_retention
	store context_management
`)

type unsupportedProviderParams struct{ error }

func prepareChatParams(fields map[string]any, candidate Candidate, globalDrop bool) (map[string]any, []string, error) {
	drop := globalDrop || shouldDropParams(fields, candidate)
	allowed := parameterList(fields, "allowed_openai_params", candidate.AllowedOpenAIParams)
	additional := parameterList(fields, "additional_drop_params", candidate.AdditionalDropParams)
	payload := clone(fields)
	delete(payload, "drop_params")
	delete(payload, "allowed_openai_params")
	delete(payload, "additional_drop_params")
	delete(payload, "metadata")
	delete(payload, "litellm_metadata")
	delete(payload, "tags")

	var nested []string
	for _, name := range additional {
		switch name {
		case "model", "messages":
			continue
		}
		if strings.ContainsAny(name, ".[") {
			nested = append(nested, name)
			continue
		}
		delete(payload, name)
		if extra, ok := payload["extra_body"].(map[string]any); ok {
			delete(extra, name)
		}
	}

	var unsupported []string
	if candidate.SupportedOpenAIParams != nil {
		for _, name := range optionalChatParams {
			value := payload[name]
			if value == nil || (name == "n" && value == float64(1)) ||
				slices.Contains(candidate.SupportedOpenAIParams, name) || slices.Contains(allowed, name) {
				continue
			}
			delete(payload, name)
			unsupported = append(unsupported, name)
		}
	}
	if len(unsupported) > 0 && !drop {
		return nil, nil, unsupportedProviderParams{fmt.Errorf(
			"%s does not support parameters: %s, for model=%s. Set drop_params to true to drop them or use allowed_openai_params to pass them through",
			candidate.Provider, strings.Join(unsupported, ", "), candidate.ModelID)}
	}
	if candidate.Provider == "openai" && candidate.Endpoint == "chat-completions" {
		if err := applyOpenAIReasoningChatParams(payload, candidate, drop); err != nil {
			return nil, nil, err
		}
	}
	return payload, nested, nil
}

func shouldDropParams(fields map[string]any, candidate Candidate) bool {
	if value, exists := fields["drop_params"]; exists {
		drop, _ := value.(bool)
		return drop
	}
	return candidate.DropParams != nil && *candidate.DropParams
}

func applyOpenAIReasoningChatParams(payload map[string]any, candidate Candidate, drop bool) error {
	model := candidate.ModelID
	oSeries := candidate.SupportedOpenAIParams != nil && len(model) > 1 && model[0] == 'o' && model[1] >= '0' && model[1] <= '9'
	gptFive := isOpenAIGPTFive(candidate)
	if !oSeries && !gptFive {
		return nil
	}
	if maxTokens := payload["max_tokens"]; maxTokens != nil {
		if payload["max_completion_tokens"] == nil {
			payload["max_completion_tokens"] = maxTokens
		}
		delete(payload, "max_tokens")
	}
	if oSeries {
		if temperature := payload["temperature"]; temperature != nil && temperature != float64(1) {
			if !drop {
				return unsupportedProviderParams{fmt.Errorf("O-series models don't support temperature=%v. Only temperature=1 is supported. To drop unsupported openai params from the call, set `litellm.drop_params = True`", temperature)}
			}
			delete(payload, "temperature")
		}
		return nil
	}
	return applyGPTFiveChatParams(payload, candidate, drop)
}

func parameterList(fields map[string]any, key string, defaults []string) []string {
	value, exists := fields[key]
	if !exists {
		return defaults
	}
	list, _ := value.([]any)
	var names []string
	for _, item := range list {
		if name, ok := item.(string); ok {
			names = append(names, name)
		}
	}
	return names
}

var chatParameterPath = regexp.MustCompile(`[^.\[]+|\[[^\]]*\]`)

func dropNestedChatParams(payload map[string]any, paths []string) {
	for _, path := range paths {
		segments := chatParameterPath.FindAllString(path, -1)
		if len(segments) < 2 || segments[0] == "messages" {
			continue
		}
		deleteChatParam(payload, segments)
	}
}

func deleteChatParam(value any, segments []string) {
	if len(segments) == 0 {
		return
	}
	part, remainder := segments[0], segments[1:]
	switch node := value.(type) {
	case map[string]any:
		if strings.HasPrefix(part, "[") {
			return
		}
		if len(remainder) == 0 {
			delete(node, part)
			return
		}
		deleteChatParam(node[part], remainder)
	case []any:
		if len(remainder) == 0 || !strings.HasPrefix(part, "[") || !strings.HasSuffix(part, "]") {
			return
		}
		if part == "[*]" {
			for _, child := range node {
				deleteChatParam(child, remainder)
			}
			return
		}
		index, err := strconv.Atoi(part[1 : len(part)-1])
		if err == nil && index >= 0 && index < len(node) {
			deleteChatParam(node[index], remainder)
		}
	}
}
