package proxyconfig

import "net/http"

type routerPatch struct {
	Fallbacks         *[]map[string][]string `json:"fallbacks,omitempty"`
	DefaultFallbacks  *[]string              `json:"default_fallbacks,omitempty"`
	ContextFallbacks  *[]map[string][]string `json:"context_window_fallbacks,omitempty"`
	PolicyFallbacks   *[]map[string][]string `json:"content_policy_fallbacks,omitempty"`
}

func (patch routerPatch) apply(config Config) Config {
	if patch.Fallbacks != nil {
		config.fallbacks = *patch.Fallbacks
	}
	if patch.DefaultFallbacks != nil {
		config.defaultFallbacks = *patch.DefaultFallbacks
	}
	if patch.ContextFallbacks != nil {
		config.contextFallbacks = *patch.ContextFallbacks
	}
	if patch.PolicyFallbacks != nil {
		config.policyFallbacks = *patch.PolicyFallbacks
	}
	return config
}

func (patch routerPatch) merge(next routerPatch) routerPatch {
	if next.Fallbacks != nil {
		patch.Fallbacks = next.Fallbacks
	}
	if next.DefaultFallbacks != nil {
		patch.DefaultFallbacks = next.DefaultFallbacks
	}
	if next.ContextFallbacks != nil {
		patch.ContextFallbacks = next.ContextFallbacks
	}
	if next.PolicyFallbacks != nil {
		patch.PolicyFallbacks = next.PolicyFallbacks
	}
	return patch
}

func (registry *modelRegistry) updateRouter(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	var input struct {
		Router *routerPatch `json:"router_settings"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil ||
		input.Router == nil || *input.Router == (routerPatch{}) {
		modelError(w, http.StatusBadRequest, "Supply supported router settings")
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.publish(w, registry.models, registry.credentials, registry.router.merge(*input.Router)) {
		modelJSON(w, http.StatusOK, map[string]string{"message": "Config updated successfully"})
	}
}
