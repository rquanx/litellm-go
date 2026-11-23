package proxyconfig

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
)

type configuredPrompt struct {
	ID     string         `yaml:"prompt_id"`
	Params map[string]any `yaml:"litellm_params"`
	Info   map[string]any `yaml:"prompt_info"`
}

func compileConfiguredPrompts(entries []configuredPrompt, configDirectory string) (map[string]promptRecord, error) {
	prompts := make(map[string]promptRecord, len(entries))
	for _, entry := range entries {
		if _, exists := prompts[entry.ID]; exists || entry.ID == "" || entry.Params == nil {
			return nil, errors.New("invalid or duplicate configured prompt")
		}
		switch entry.Params["prompt_integration"] {
		case "dotprompt":
			if file, ok := entry.Params["prompt_file"].(string); ok && file != "" {
				content, err := readConfiguredPromptFile(configDirectory, file)
				if err != nil {
					return nil, fmt.Errorf("could not load prompt file for %q: %w", entry.ID, err)
				}
				entry.Params["dotprompt_content"] = content
				delete(entry.Params, "prompt_file")
			}
		case "generic_prompt_management", "langfuse", "gitlab", "bitbucket", "arize_phoenix":
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

func readConfiguredPromptFile(configDirectory, filename string) (string, error) {
	if configDirectory == "" || filepath.Ext(filename) != ".prompt" {
		return "", errors.New("prompt_file requires a .prompt file and a configuration path")
	}
	if !filepath.IsAbs(filename) {
		filename = filepath.Join(configDirectory, filename)
	}
	file, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("prompt_file must be a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxManagedPromptBytes+1))
	if err != nil || len(raw) > maxManagedPromptBytes {
		return "", errors.New("could not read prompt file within the size limit")
	}
	if _, _, err := splitDotprompt(string(raw)); err != nil {
		return "", err
	}
	return string(raw), nil
}

func (api promptAPI) staticPrompt(id, environment string, version int, admin bool, scope []string) (promptRecord, bool) {
	prompt, exists := api.static[id]
	return prompt, exists && (environment == "" || environment == prompt.Environment) && (version == 0 || version == 1) &&
		(admin || slices.Contains(scope, id) || slices.Contains(scope, id+".v1"))
}
