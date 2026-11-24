package proxyconfig

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
)

func (registry *modelRegistry) manageFallback(w http.ResponseWriter, r *http.Request) {
	if !registry.allowed(w, r) {
		return
	}
	input := struct {
		Model  string   `json:"model"`
		Models []string `json:"fallback_models,omitempty"`
		Type   string   `json:"fallback_type"`
	}{Model: r.PathValue("model"), Type: r.URL.Query().Get("fallback_type")}
	if r.Method == http.MethodPost {
		if err := decodeModelJSON(http.MaxBytesReader(w, r.Body, 1<<20), &input); err != nil ||
			strings.TrimSpace(input.Model) == "" || len(input.Models) == 0 {
			modelError(w, http.StatusUnprocessableEntity, "Supply a model and a nonempty fallback_models list")
			return
		}
		input.Model = strings.TrimSpace(input.Model)
	}
	if input.Type == "" {
		input.Type = "general"
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	patch := registry.router
	config := patch.apply(registry.base)
	var entries []map[string][]string
	var destination **[]map[string][]string
	switch input.Type {
	case "general":
		entries, destination = config.fallbacks, &patch.Fallbacks
	case "context_window":
		entries, destination = config.contextFallbacks, &patch.ContextFallbacks
	case "content_policy":
		entries, destination = config.policyFallbacks, &patch.PolicyFallbacks
	default:
		modelError(w, http.StatusUnprocessableEntity, "Invalid fallback_type")
		return
	}
	index := slices.IndexFunc(entries, func(entry map[string][]string) bool {
		_, exists := entry[input.Model]
		return exists
	})
	missing := fmt.Sprintf("No %s fallbacks configured for model '%s'", input.Type, input.Model)
	if r.Method == http.MethodGet {
		if index >= 0 {
			input.Models = entries[index][input.Model]
		} else {
			for _, entry := range entries {
				if models, exists := entry["*"]; exists {
					input.Models = models
					break
				}
			}
		}
		if len(input.Models) == 0 {
			fallbackError(w, http.StatusNotFound, missing)
			return
		}
		modelJSON(w, http.StatusOK, input)
		return
	}
	next := make([]map[string][]string, 0, len(entries)+1)
	next = append(next, entries...)
	var message string
	if r.Method == http.MethodDelete {
		if index < 0 {
			fallbackError(w, http.StatusNotFound, missing)
			return
		}
		replacement := maps.Clone(next[index])
		delete(replacement, input.Model)
		if len(replacement) == 0 {
			next = slices.Delete(next, index, index+1)
		} else {
			next[index] = replacement
		}
		message = "Fallback configuration deleted successfully"
	} else {
		aliases := make(map[string]bool)
		for _, models := range [][]deployment{registry.base.models, registry.models} {
			for _, model := range models {
				aliases[model.Alias] = true
			}
		}
		for alias := range config.modelGroupAliases {
			aliases[alias] = true
		}
		if !aliases[input.Model] {
			fallbackError(w, http.StatusNotFound, fmt.Sprintf("Model '%s' not found in router", input.Model))
			return
		}
		seen := make(map[string]bool, len(input.Models))
		for _, model := range input.Models {
			if seen[model] {
				modelError(w, http.StatusUnprocessableEntity, "fallback_models must not contain duplicates")
				return
			}
			if !aliases[model] || model == input.Model {
				fallbackError(w, http.StatusBadRequest, fmt.Sprintf("Invalid fallback model: %s", model))
				return
			}
			seen[model] = true
		}
		if index >= 0 {
			next[index] = maps.Clone(next[index])
			next[index][input.Model] = input.Models
			message = "Fallback configuration updated successfully"
		} else {
			next = append(next, map[string][]string{input.Model: input.Models})
			message = "Fallback configuration created successfully"
		}
	}
	*destination = &next
	if registry.publish(w, registry.models, registry.credentials, patch) {
		response := map[string]any{"model": input.Model, "fallback_type": input.Type, "message": message}
		if r.Method == http.MethodPost {
			response["fallback_models"] = input.Models
		}
		modelJSON(w, http.StatusOK, response)
	}
}

func fallbackError(w http.ResponseWriter, status int, message string) {
	modelJSON(w, status, map[string]any{"detail": map[string]string{"error": message}})
}
