package proxyconfig

import (
	"strings"
	"testing"
)

func TestDeploymentProviderResolutionMatchesPinnedLiteLLM(t *testing.T) {
	config, err := Load(strings.NewReader(`
model_list:
  - model_name: openai-model
    litellm_params: {model: gpt-4o, api_base: http://fixture.invalid/v1}
  - model_name: anthropic-model
    litellm_params: {model: claude-sonnet-4-5, api_base: http://fixture.invalid/v1}
  - model_name: nested-model
    litellm_params: {model: owner/model, custom_llm_provider: openai_like, api_base: http://fixture.invalid/v1}
`))
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string][2]string{
		"openai-model": {"openai", "gpt-4o"},
		"anthropic-model": {"anthropic", "claude-sonnet-4-5"},
		"nested-model": {"openai_like", "owner/model"},
	}
	for _, route := range config.Routes {
		want := expected[route.Alias]
		if candidate := route.Candidates[0]; candidate.Provider != want[0] || candidate.ModelID != want[1] {
			t.Fatalf("incorrect provider and model for %s: %s %s", route.Alias, candidate.Provider, candidate.ModelID)
		}
	}
}
