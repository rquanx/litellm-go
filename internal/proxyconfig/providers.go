package proxyconfig

import (
	_ "embed"
	"encoding/json"
	"slices"
)

//go:embed openai-compatible.json
var openAICompatibleJSON []byte

//go:embed openai-provider-settings.json
var openAIProviderSettingsJSON []byte

var openAICompatibleProviders = loadProviderNames()
var openAIProviderSettings = loadProviderSettings()

type providerSettings struct {
	BaseURL       string            `json:"base_url"`
	APIKeyEnv     string            `json:"api_key_env"`
	APIBaseEnv    string            `json:"api_base_env"`
	ParamMappings map[string]string `json:"param_mappings"`
	Headers       map[string]string `json:"headers"`
	Special       struct {
		TextOnly bool `json:"convert_content_list_to_string"`
	} `json:"special_handling"`
}

func loadProviderSettings() map[string]providerSettings {
	var result map[string]providerSettings
	if err := json.Unmarshal(openAIProviderSettingsJSON, &result); err != nil {
		panic("invalid embedded LiteLLM provider settings")
	}
	return result
}

func loadProviderNames() []string {
	var names []string
	if err := json.Unmarshal(openAICompatibleJSON, &names); err != nil {
		panic("invalid embedded LiteLLM provider list")
	}
	return names
}

func supportsOpenAITransport(provider string) bool {
	switch provider {
	case "openai", "openai_like":
		return true
	case "chatgpt", "github_copilot":
		return false
	default:
		_, configured := openAIProviderSettings[provider]
		return configured || slices.Contains(openAICompatibleProviders, provider)
	}
}
