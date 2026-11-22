package runtime

import (
	"errors"
	"fmt"
)

func fallbackNames(value any, alias string) ([]string, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, errors.New("fallbacks must be an array")
	}
	result := make([]string, 0, len(items))
	var wildcard []string
	matched := false
	for _, entry := range items {
		switch item := entry.(type) {
		case string:
			result = append(result, item)
			matched = true
		case map[string]any:
			for name, raw := range item {
				models, ok := raw.([]any)
				if !ok {
					return nil, errors.New("fallback model list must be an array")
				}
				names := make([]string, 0, len(models))
				for _, model := range models {
					target, ok := model.(string)
					if !ok || target == "" {
						return nil, errors.New("fallback model must be a nonempty alias")
					}
					names = append(names, target)
				}
				switch name {
				case alias:
					result = append(result, names...)
					matched = true
				case "*":
					wildcard = append(wildcard, names...)
				}
			}
		default:
			return nil, errors.New("invalid fallback entry")
		}
	}
	if !matched && wildcard != nil {
		return wildcard, nil
	}
	return result, nil
}

func (s *Server) requestFallbacks(route Route, input map[string]any, scope []string, endpoint string) (Route, error) {
	override, exists := input["router_settings_override"]
	settings, valid := override.(map[string]any)
	if exists && override != nil && !valid {
		return Route{}, errors.New("router_settings_override must be an object")
	}
	delete(input, "router_settings_override")
	for _, field := range []struct {
		name string
		destination *[]Candidate
	}{
		{"fallbacks", &route.Candidates},
		{"context_window_fallbacks", &route.ContextFallback},
		{"content_policy_fallbacks", &route.PolicyFallback},
	} {
		value, found := input[field.name]
		delete(input, field.name)
		if item, ok := settings[field.name]; ok {
			value, found = item, true
			delete(settings, field.name)
		}
		if !found || value == nil {
			continue
		}
		names, err := fallbackNames(value, route.Alias)
		if err != nil {
			return Route{}, err
		}
		if len(names) > 16 {
			return Route{}, errors.New("too many fallback models")
		}
		candidates := make([]Candidate, 0, len(names))
		if field.name == "fallbacks" {
			for _, primary := range route.Candidates {
				if primary.RoutingGroup == route.Alias || primary.RoutingGroup == "" {
					candidates = append(candidates, primary)
				}
			}
		}
		seen := map[string]bool{route.Alias: true}
		s.mu.RLock()
		for _, target := range names {
			if target == "" || seen[target] || !modelAllowed(scope, target) {
				s.mu.RUnlock()
				return Route{}, errors.New("invalid or unavailable fallback alias")
			}
			seen[target] = true
			backup, known := s.configuredRoutes[target]
			if !known {
				s.mu.RUnlock()
				return Route{}, errors.New("invalid or unavailable fallback alias")
			}
			start := len(candidates)
			for _, candidate := range backup.Candidates {
				if candidate.RoutingGroup == target && servesEndpoint(endpoint, candidate.Endpoint) {
					candidates = append(candidates, candidate)
				}
			}
			if len(candidates) == start || len(candidates) > 16 {
				s.mu.RUnlock()
				return Route{}, fmt.Errorf("invalid fallback deployment count for %q", target)
			}
		}
		s.mu.RUnlock()
		*field.destination = candidates
	}
	if len(settings) != 0 {
		return Route{}, errors.New("unsupported router settings override")
	}
	return route, nil
}
