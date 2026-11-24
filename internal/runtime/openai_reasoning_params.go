package runtime

import (
	"fmt"
	"slices"
	"strings"
)

type OpenAIReasoningCapabilities struct {
	None bool
	HighExtended bool
	MinimalDisabled bool
	LowDisabled bool
}

func isOpenAIGPTFive(candidate Candidate) bool {
	return candidate.Provider == "openai" && strings.HasPrefix(candidate.ModelID, "gpt-5") &&
		!strings.HasPrefix(candidate.ModelID, "gpt-5-chat")
}

func applyGPTFiveChatParams(payload map[string]any, candidate Candidate, drop bool) error {
	if strings.Contains(candidate.ModelID, "search") {
		return nil
	}
	capabilities := candidate.ReasoningCapabilities
	effort, _ := payload["reasoning_effort"].(string)
	if setting, ok := payload["reasoning_effort"].(map[string]any); ok {
		if level, ok := setting["effort"].(string); ok {
			effort = level
			payload["reasoning_effort"] = level
		}
	}
	if (effort == "xhigh" && !capabilities.HighExtended) ||
		(effort == "minimal" && capabilities.MinimalDisabled) || (effort == "low" && capabilities.LowDisabled) {
		if !drop {
			return unsupportedProviderParams{fmt.Errorf("reasoning_effort=%s is not supported for this model", effort)}
		}
		delete(payload, "reasoning_effort")
	}
	flexibleSampling := capabilities.None && (effort == "" || effort == "none")
	if capabilities.None && !flexibleSampling {
		for _, name := range []string{"logprobs", "top_logprobs", "top_p"} {
			if payload[name] == nil {
				continue
			}
			if !drop {
				return unsupportedProviderParams{fmt.Errorf("gpt-5 only supports %s with reasoning_effort='none'", name)}
			}
			delete(payload, name)
		}
	}
	if temperature := payload["temperature"]; temperature != nil && temperature != float64(1) && !flexibleSampling {
		if !drop {
			return gptFiveTemperatureError(temperature)
		}
		delete(payload, "temperature")
	}
	return nil
}

func gptFiveTemperatureError(temperature any) error {
	return unsupportedProviderParams{fmt.Errorf("gpt-5 models don't support temperature=%v. Only temperature=1 is supported unless the model supports reasoning effort 'none' and uses it", temperature)}
}

var optionalResponsesParams = strings.Fields(`
	include instructions max_output_tokens metadata parallel_tool_calls previous_response_id
	reasoning store background stream temperature text tool_choice tools top_p truncation user
	service_tier safety_identifier prompt max_tool_calls prompt_cache_key prompt_cache_retention
	stream_options top_logprobs partial_images context_management
`)

func prepareOpenAIResponsesParams(payload map[string]any, candidate Candidate, globalDrop bool) error {
	for _, name := range parameterList(payload, "additional_drop_params", candidate.AdditionalDropParams) {
		if slices.Contains(optionalResponsesParams, name) {
			delete(payload, name)
		}
	}
	delete(payload, "drop_params")
	delete(payload, "allowed_openai_params")
	delete(payload, "additional_drop_params")
	delete(payload, "litellm_metadata")
	delete(payload, "tags")
	if !isOpenAIGPTFive(candidate) {
		return nil
	}
	reasoning, _ := payload["reasoning"].(map[string]any)
	effort, _ := reasoning["effort"].(string)
	flexibleTemperature := candidate.ReasoningCapabilities.None && (effort == "" || effort == "none")
	if temperature := payload["temperature"]; temperature != nil && temperature != float64(1) && !flexibleTemperature {
		if !globalDrop {
			return gptFiveTemperatureError(temperature)
		}
		delete(payload, "temperature")
	}
	return nil
}
