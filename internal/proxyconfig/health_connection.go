package proxyconfig

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/rquanx/minato-runtime/internal/runtime"
)

func (registry *modelRegistry) testModelConnection(w http.ResponseWriter, r *http.Request) {
	if registry.token == "" || subtle.ConstantTimeCompare([]byte(registry.token), []byte(runtime.ProxyCredential(r))) != 1 {
		registry.server.ServeHTTP(w, r)
		return
	}

	var request struct {
		Mode   string          `json:"mode"`
		Params json.RawMessage `json:"litellm_params"`
		Info   json.RawMessage `json:"model_info"`
	}
	if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &request); err != nil {
		modelError(w, http.StatusBadRequest, "Invalid connection test request")
		return
	}
	var params map[string]any
	if (len(request.Params) != 0 && json.Unmarshal(request.Params, &params) != nil) || hasConnectionEnvRef(params) {
		modelError(w, http.StatusBadRequest, "Invalid parameters for connection testing")
		return
	}
	var info struct {
		ID string `json:"id"`
	}
	if len(request.Info) != 0 && json.Unmarshal(request.Info, &info) != nil {
		modelError(w, http.StatusBadRequest, "Invalid model_info")
		return
	}
	model, _ := params["model"].(string)
	if model == "" && info.ID == "" {
		modelError(w, http.StatusBadRequest, "A model or deployment ID is required")
		return
	}

	entry, credentials := registry.connectionDeployment(info.ID, model)
	if info.ID != "" && entry.Alias == "" && model == "" {
		modelError(w, http.StatusNotFound, "Unknown deployment")
		return
	}
	configuredModel := entry.Params.Model
	if entry.Alias == "" {
		entry.Alias = "test_model"
	}
	if len(request.Params) != 0 && json.Unmarshal(request.Params, &entry.Params) != nil {
		modelError(w, http.StatusBadRequest, "Invalid provider parameters")
		return
	}
	if configuredModel != "" && (model == "" || model == entry.Alias) {
		entry.Params.Model = configuredModel
	} else if configuredModel != "" {
		provider, _, _ := strings.Cut(configuredModel, "/")
		override, _, found := strings.Cut(entry.Params.Model, "/")
		if found && override != provider {
			modelError(w, http.StatusBadRequest, "The model provider must match the selected deployment")
			return
		}
		if !found {
			entry.Params.Model = provider + "/" + entry.Params.Model
		}
	}
	if len(request.Info) != 0 && json.Unmarshal(request.Info, &entry.Info) != nil {
		modelError(w, http.StatusBadRequest, "Invalid model_info")
		return
	}
	if request.Mode != "" {
		entry.Info.Mode = request.Mode
		entry.Info.Endpoints = nil
	}
	entry, err := entry.withCredential(credentials)
	if err != nil {
		modelError(w, http.StatusBadRequest, "Invalid provider credential")
		return
	}
	candidate, err := entry.candidate()
	if err != nil {
		modelError(w, http.StatusBadRequest, "Invalid provider connection configuration")
		return
	}
	registry.server.TestModelDeployment(w, r, candidate)
}

func (registry *modelRegistry) connectionDeployment(id, model string) (deployment, map[string]providerCredential) {
	registry.mu.Lock()
	defer registry.mu.Unlock()

	credentials := combinedCredentials(registry.base.credentials, registry.credentials)
	var selected deployment
	for _, entries := range [][]deployment{registry.base.models, registry.models} {
		for _, entry := range entries {
			if id != "" && entry.Info.ID == id {
				return entry, credentials
			}
			if model != "" && entry.Alias == model && (selected.Alias == "" || selected.Alias != model) {
				selected = entry
			} else if model != "" && entry.Params.Model == model && selected.Alias == "" {
				selected = entry
			}
		}
	}
	return selected, credentials
}

func hasConnectionEnvRef(value any) bool {
	switch item := value.(type) {
	case string:
		return strings.HasPrefix(strings.TrimSpace(item), "os.environ/")
	case map[string]any:
		for _, field := range item {
			if hasConnectionEnvRef(field) {
				return true
			}
		}
	case []any:
		for _, field := range item {
			if hasConnectionEnvRef(field) {
				return true
			}
		}
	}
	return false
}
