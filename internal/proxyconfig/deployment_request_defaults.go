package proxyconfig

import "encoding/json"

type deploymentRequestDefaults struct {
	Temperature *float64 `yaml:"temperature" json:"temperature,omitempty"`
	TopP *float64 `yaml:"top_p" json:"top_p,omitempty"`
	TopK *int `yaml:"top_k" json:"top_k,omitempty"`
	MaxTokens *int `yaml:"max_tokens" json:"max_tokens,omitempty"`
	MaxCompletionTokens *int `yaml:"max_completion_tokens" json:"max_completion_tokens,omitempty"`
	MaxOutputTokens *int `yaml:"max_output_tokens" json:"max_output_tokens,omitempty"`
	N *int `yaml:"n" json:"n,omitempty"`
	Seed *int `yaml:"seed" json:"seed,omitempty"`
	Stop any `yaml:"stop" json:"stop,omitempty"`
	FrequencyPenalty *float64 `yaml:"frequency_penalty" json:"frequency_penalty,omitempty"`
	PresencePenalty *float64 `yaml:"presence_penalty" json:"presence_penalty,omitempty"`
	LogitBias map[string]any `yaml:"logit_bias" json:"logit_bias,omitempty"`
	Logprobs *bool `yaml:"logprobs" json:"logprobs,omitempty"`
	TopLogprobs *int `yaml:"top_logprobs" json:"top_logprobs,omitempty"`
	ReasoningEffort any `yaml:"reasoning_effort" json:"reasoning_effort,omitempty"`
	Reasoning map[string]any `yaml:"reasoning" json:"reasoning,omitempty"`
	Thinking map[string]any `yaml:"thinking" json:"thinking,omitempty"`
	Verbosity string `yaml:"verbosity" json:"verbosity,omitempty"`
	ResponseFormat map[string]any `yaml:"response_format" json:"response_format,omitempty"`
	Text map[string]any `yaml:"text" json:"text,omitempty"`
	Tools []any `yaml:"tools" json:"tools,omitempty"`
	ToolChoice any `yaml:"tool_choice" json:"tool_choice,omitempty"`
	ParallelToolCalls *bool `yaml:"parallel_tool_calls" json:"parallel_tool_calls,omitempty"`
	ServiceTier string `yaml:"service_tier" json:"service_tier,omitempty"`
	ExtraBody map[string]any `yaml:"extra_body" json:"extra_body,omitempty"`
}

func requestDefaultFields(defaults any) (map[string]any, error) {
	data, err := json.Marshal(defaults)
	if err != nil || len(data) == 2 {
		return nil, err
	}
	var fields map[string]any
	err = json.Unmarshal(data, &fields)
	return fields, err
}
