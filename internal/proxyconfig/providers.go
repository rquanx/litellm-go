package proxyconfig

import (
	_ "embed"
	"encoding/json"
	"slices"
)

//go:embed openai-compatible.json
var openAICompatibleJSON []byte

var openAICompatibleProviders = loadProviderNames()

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
		return slices.Contains(openAICompatibleProviders, provider)
	}
}
