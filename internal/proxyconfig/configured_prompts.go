package proxyconfig

import (
	"errors"
	"slices"
)

type configuredPrompt struct {
	ID     string         `yaml:"prompt_id"`
	Params map[string]any `yaml:"litellm_params"`
	Info   map[string]any `yaml:"prompt_info"`
}

func compileConfiguredPrompts(entries []configuredPrompt) (map[string]promptRecord, error) {
	prompts := make(map[string]promptRecord, len(entries))
	for _, entry := range entries {
		if _, exists := prompts[entry.ID]; exists || entry.ID == "" || entry.Params == nil {
			return nil, errors.New("invalid or duplicate configured prompt")
		}
		switch entry.Params["prompt_integration"] {
		case "dotprompt", "generic_prompt_management", "langfuse", "gitlab", "bitbucket", "arize_phoenix":
		default:
			return nil, errors.New("unsupported configured prompt integration")
		}
		if entry.Info == nil {
			entry.Info = make(map[string]any)
		}
		entry.Info["prompt_type"] = "config"
		env, _ := entry.Info["environment"].(string)
		if env == "" {
			env = "development"
		}
		prompts[entry.ID] = promptRecord{PromptID: entry.ID, Version: 1, Environment: env, Params: entry.Params, Info: entry.Info}
	}
	return prompts, nil
}

func (api promptAPI) staticPrompt(id, environment string, version int, admin bool, scope []string) (promptRecord, bool) {
	prompt, exists := api.static[id]
	return prompt, exists && (environment == "" || environment == prompt.Environment) && (version == 0 || version == 1) &&
		(admin || slices.Contains(scope, id) || slices.Contains(scope, id+".v1"))
}
