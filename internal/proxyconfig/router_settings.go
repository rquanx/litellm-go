package proxyconfig

import (
	"maps"
	"net/http"
	"slices"
	"time"
)

var routingStrategies = map[string]string{
	"simple-shuffle": "Random selection with deployment weights",
	"least-busy": "Fewest active requests",
	"latency-based-routing": "Lowest recent latency",
	"cost-based-routing": "Lowest token price",
	"usage-based-routing": "Lowest local token usage",
	"usage-based-routing-v2": "Lowest local token usage with capacity reservation",
}

func (registry *modelRegistry) routerSettings(w http.ResponseWriter, r *http.Request) {
	if !registry.keys.adminViewer(r) && !registry.allowed(w, r) {
		return
	}
	registry.mu.Lock()
	config := registry.router.apply(registry.base)
	registry.mu.Unlock()
	values := map[string]any{
		"routing_strategy": config.RoutingStrategy,
		"timeout": config.Timeout.Seconds(),
		"num_retries": config.NumRetries,
		"retry_after": config.RetryAfter.Seconds(),
		"allowed_fails": config.AllowedFails,
		"cooldown_time": config.Cooldown.Seconds(),
		"disable_cooldowns": config.Cooldown == 0,
		"fallbacks": config.fallbacks,
		"default_fallbacks": config.defaultFallbacks,
		"context_window_fallbacks": config.contextFallbacks,
		"content_policy_fallbacks": config.policyFallbacks,
	}
	fields := []struct {
		name, kind, label, description string
		fallback any
	}{
		{"routing_strategy", "String", "Routing Strategy", "Deployment selection logic", "simple-shuffle"},
		{"timeout", "Float", "Timeout", "Upstream request timeout in seconds", 120},
		{"num_retries", "Integer", "Retries", "Retry budget per inference request", 0},
		{"retry_after", "Integer", "Retry After", "Minimum computed retry delay in seconds", 0},
		{"allowed_fails", "Integer", "Allowed Failures", "Failure count before cooling down a deployment", 0},
		{"cooldown_time", "Float", "Cooldown Time", "Deployment cooldown in seconds", 60},
		{"disable_cooldowns", "Boolean", "Disable Cooldowns", "Whether deployment cooldown is disabled", true},
		{"fallbacks", "List", "Fallbacks", "Ordered fallback model groups", nil},
		{"default_fallbacks", "List", "Default Fallbacks", "Fallback model groups shared by configured models", nil},
		{"context_window_fallbacks", "List", "Context Window Fallbacks", "Targets for context length failures", nil},
		{"content_policy_fallbacks", "List", "Content Policy Fallbacks", "Targets for recognized content policy errors", nil},
	}
	if config.Timeout == 0 {
		values["timeout"] = (120 * time.Second).Seconds()
	}
	includeValues := r.URL.Path == "/router/settings"
	result := make([]map[string]any, 0, len(fields))
	for _, field := range fields {
		item := map[string]any{
			"field_name": field.name, "field_type": field.kind, "ui_field_name": field.label,
			"field_description": field.description, "field_default": field.fallback, "field_value": nil,
			"options": nil, "link": nil,
		}
		if includeValues {
			item["field_value"] = values[field.name]
		}
		if field.name == "routing_strategy" {
			item["options"] = slices.Sorted(maps.Keys(routingStrategies))
		}
		result = append(result, item)
	}
	response := map[string]any{"fields": result, "routing_strategy_descriptions": routingStrategies}
	if includeValues {
		response["current_values"] = values
	}
	modelJSON(w, http.StatusOK, response)
}
