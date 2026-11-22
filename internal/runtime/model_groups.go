package runtime

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"time"
)

func (s *Server) PublicModelGroups(ctx context.Context, names []string, metadata func(Candidate) (map[string]any, []string)) []map[string]any {
	result := make([]map[string]any, 0, len(names))
	if len(names) == 0 {
		return result
	}
	seen := make(map[string]bool, len(names))
	s.mu.RLock()
	routes := make(map[string]Route, len(names))
	for _, name := range names {
		if route, ok := s.configuredRoutes[name]; ok && !route.Hidden {
			routes[name] = route
		}
	}
	s.mu.RUnlock()
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		group := map[string]any{
			"model_group": name, "providers": []string{}, "mode": "chat",
			"max_input_tokens": nil, "max_output_tokens": nil, "input_cost_per_pixel": nil,
			"input_cost_per_token": nil, "output_cost_per_token": nil, "rpm": nil, "tpm": nil,
			"supports_parallel_function_calling": false, "supports_vision": false, "supports_web_search": false,
			"supports_url_context": false, "supports_reasoning": false, "supports_function_calling": false,
			"supported_openai_params": []string{}, "configurable_clientside_auth_params": nil,
			"is_public_model_group": true, "health_status": nil, "health_response_time": nil, "health_checked_at": nil,
		}
		if route, ok := routes[name]; ok {
			addPublicDeployments(group, route, metadata)
		}
		result = append(result, group)
	}
	if s.config.HealthHistory != nil {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		records, _ := s.config.HealthHistory.Read(ctx, HealthHistoryQuery{Latest: true, Scope: names})
		latest := make(map[string]ModelHealthRecord, len(records))
		for _, record := range records {
			if previous, exists := latest[record.Model]; !exists || record.CheckedAt.After(previous.CheckedAt) {
				latest[record.Model] = record
			}
		}
		for _, group := range result {
			if record, ok := latest[group["model_group"].(string)]; ok {
				group["health_status"] = record.Status
				group["health_response_time"] = record.ResponseMS
				group["health_checked_at"] = record.CheckedAt
			}
		}
	}
	return result
}

func addPublicDeployments(group map[string]any, route Route, metadata func(Candidate) (map[string]any, []string)) {
	providers := []string{}
	var costs [2]*float64
	var rpm, tpm *int
	for _, candidate := range route.Candidates {
		if candidate.RoutingGroup != "" && candidate.RoutingGroup != route.Alias {
			continue
		}
		fields, params := metadata(candidate)
		if len(providers) == 0 {
			group["mode"] = modelGroupMode(candidate.Endpoint)
			if mode, ok := fields["mode"].(string); ok {
				group["mode"] = mode
			}
			if pixelPrice := fields["input_cost_per_pixel"]; pixelPrice != nil {
				group["input_cost_per_pixel"] = pixelPrice
			}
		}
		for _, key := range []string{"max_input_tokens", "max_output_tokens"} {
			if value, ok := fields[key].(float64); ok {
				if prior, set := group[key].(float64); !set || value > prior {
					group[key] = value
				}
			}
		}
		for _, key := range []string{"supports_vision", "supports_function_calling",
			"supports_web_search", "supports_reasoning", "supports_url_context"} {
			if fields[key] == true {
				group[key] = true
			}
		}
		if params != nil {
			group["supported_openai_params"] = params
		}
		if !slices.Contains(providers, candidate.Provider) {
			providers = append(providers, candidate.Provider)
		}
		for index, price := range candidate.UsagePrice {
			if price != nil && (costs[index] == nil || *price > *costs[index]) {
				costs[index] = price
			}
		}
		if candidate.RPM != nil {
			if rpm == nil {
				rpm = new(int)
			}
			*rpm += *candidate.RPM
		}
		if candidate.TPM != nil {
			if tpm == nil {
				tpm = new(int)
			}
			*tpm += *candidate.TPM
		}
	}
	group["providers"] = providers
	group["rpm"], group["tpm"] = rpm, tpm
	group["input_cost_per_token"], group["output_cost_per_token"] = costs[0], costs[1]
}

func modelGroupMode(endpoint string) string {
	switch endpoint {
	case "embeddings", "google-embeddings", "bedrock-embeddings", "cohere-embeddings":
		return "embedding"
	case "completions":
		return "completion"
	case "image-generation", "image-edit":
		return "image_generation"
	case "transcription":
		return "audio_transcription"
	case "speech":
		return "audio_speech"
	case "moderations", "rerank", "ocr":
		return endpoint
	default:
		return "chat"
	}
}

func (s *Server) modelGroups(w http.ResponseWriter, request *http.Request, scope []string) {
	selected := request.URL.Query().Get("model_group")
	s.mu.RLock()
	routes := maps.Clone(s.configuredRoutes)
	s.mu.RUnlock()
	data := make([]map[string]any, 0, len(routes))
	for _, alias := range slices.Sorted(maps.Keys(routes)) {
		route := routes[alias]
		if (selected != "" && selected != alias) || !modelAllowed(scope, alias) || (route.Hidden && selected == "") {
			continue
		}
		providers := []string{}
		var primary Candidate
		found := false
		for _, deployment := range route.Candidates {
			if deployment.RoutingGroup != "" && deployment.RoutingGroup != alias {
				continue
			}
			if !found {
				primary, found = deployment, true
			}
			if deployment.Provider != "" && !slices.Contains(providers, deployment.Provider) {
				providers = append(providers, deployment.Provider)
			}
		}
		if !found {
			continue
		}
		data = append(data, map[string]any{
			"model_group": alias, "providers": providers, "mode": modelGroupMode(primary.Endpoint), "rpm": primary.RPM, "tpm": primary.TPM,
			"input_cost_per_token": primary.UsagePrice[0], "output_cost_per_token": primary.UsagePrice[1],
		})
	}
	if len(data) == 0 && selected != "" {
		writeError(w, http.StatusNotFound, "model_not_found", "Model group not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}
