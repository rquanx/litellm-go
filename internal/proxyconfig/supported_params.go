package proxyconfig

import (
	"bytes"
	"compress/gzip"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/rquanx/litellm-go/internal/runtime"
)

//go:embed supported-openai-params.json.gz
var supportedParamsSnapshot []byte

//go:embed provider-openai-params.json.gz
var defaultProviderParams []byte

var providerParamsCatalog = sync.OnceValue(func() map[string][]string {
	reader, err := gzip.NewReader(bytes.NewReader(defaultProviderParams))
	if err != nil {
		return nil
	}
	defer reader.Close()
	var catalog map[string][]string
	if json.NewDecoder(reader).Decode(&catalog) != nil {
		return nil
	}
	return catalog
})

//go:embed provider-model-settings.json.gz
var providerModelSettings []byte

var supportedParamsCatalog = sync.OnceValue(func() map[string][]string {
	reader, err := gzip.NewReader(bytes.NewReader(supportedParamsSnapshot))
	if err != nil {
		return nil
	}
	defer reader.Close()
	var catalog map[string][]string
	if json.NewDecoder(reader).Decode(&catalog) != nil {
		return nil
	}
	return catalog
})

type reasoningMetadata struct {
	None bool `json:"supports_none_reasoning_effort"`
	XHigh bool `json:"supports_xhigh_reasoning_effort"`
	Minimal *bool `json:"supports_minimal_reasoning_effort"`
	Low *bool `json:"supports_low_reasoning_effort"`
}

var reasoningCatalog = sync.OnceValue(func() map[string]reasoningMetadata {
	reader, err := gzip.NewReader(bytes.NewReader(publicPrices))
	if err != nil {
		return nil
	}
	defer reader.Close()
	var catalog map[string]reasoningMetadata
	if json.NewDecoder(reader).Decode(&catalog) != nil {
		return nil
	}
	selected := make(map[string]reasoningMetadata)
	for model, metadata := range catalog {
		if strings.HasPrefix(strings.TrimPrefix(model, "openai/"), "gpt-5") {
			selected[model] = metadata
		}
	}
	return selected
})

func deploymentReasoningCapabilities(provider, model string) runtime.OpenAIReasoningCapabilities {
	if provider != "openai" || !strings.HasPrefix(model, "gpt-5") {
		return runtime.OpenAIReasoningCapabilities{}
	}
	catalog := reasoningCatalog()
	meta, exists := catalog[model]
	if !exists {
		meta = catalog[provider+"/"+model]
	}
	return runtime.OpenAIReasoningCapabilities{None: meta.None, HighExtended: meta.XHigh,
		MinimalDisabled: meta.Minimal != nil && !*meta.Minimal, LowDisabled: meta.Low != nil && !*meta.Low}
}

func deploymentOpenAIParams(configuredModel, provider, model string) []string {
	catalog := supportedParamsCatalog()
	if params := catalog[provider+"/"+model]; params != nil {
		return params
	}
	resolvedProvider, _, recognized := deploymentIdentity(configuredModel, "")
	if recognized && resolvedProvider == provider {
		if params := catalog[configuredModel]; params != nil {
			return params
		}
	}
	if provider == "openai" || (provider == "anthropic" && strings.HasPrefix(model, "claude-")) ||
		(provider == "gemini" && strings.HasPrefix(model, "gemini-")) {
		return catalog[model]
	}
	return nil
}

func (registry *modelRegistry) allowModelMetadata(w http.ResponseWriter, r *http.Request) bool {
	token := runtime.ProxyCredential(r)
	valid := registry.token != "" && subtle.ConstantTimeCompare([]byte(registry.token), []byte(token)) == 1
	if !valid && registry.keys != nil {
		_, valid = registry.keys.identifyResourceCaller(r)
	}
	if !valid {
		modelError(w, http.StatusUnauthorized, "Proxy token required")
	}
	return valid
}

func (registry *modelRegistry) supportedOpenAIParams(w http.ResponseWriter, r *http.Request) {
	if !registry.allowModelMetadata(w, r) {
		return
	}
	model := r.URL.Query().Get("model")
	if model == "" {
		modelJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": []map[string]any{
			{"type": "missing", "loc": []string{"query", "model"}, "msg": "Field required", "input": nil},
		}})
		return
	}
	catalog := supportedParamsCatalog()
	params, found := catalog[model]
	if !found && strings.HasPrefix(model, "openai/") {
		params, found = catalog[strings.TrimPrefix(model, "openai/")]
	}
	if !found {
		if provider, _, resolved := deploymentIdentity(model, ""); resolved {
			params, found = providerParamsCatalog()[provider]
		}
	}
	if !found {
		modelJSON(w, http.StatusBadRequest, map[string]any{"detail": map[string]string{"error": "Could not map model=" + model}})
		return
	}
	modelJSON(w, http.StatusOK, map[string]any{"supported_openai_params": params})
}
