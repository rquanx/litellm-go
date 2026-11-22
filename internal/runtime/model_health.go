package runtime

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

type healthDeployment struct {
	alias string
	candidate Candidate
}

func (s *Server) modelHealth(w http.ResponseWriter, r *http.Request, scope []string, admin bool) {
	query := r.URL.Query()
	model, id := query.Get("model"), query.Get("model_id")
	var deployments []healthDeployment
	s.mu.RLock()
	for alias, route := range s.configuredRoutes {
		if route.Hidden || !modelAllowed(scope, alias) || (model != "" && model != alias) {
			continue
		}
		seen := make(map[string]bool)
		for _, candidate := range route.Candidates {
			if (candidate.RoutingGroup != "" && candidate.RoutingGroup != alias) ||
				(id != "" && candidate.DeploymentID != id) ||
				candidate.DeploymentID == "" || seen[candidate.DeploymentID] {
				continue
			}
			seen[candidate.DeploymentID] = true
			deployments = append(deployments, healthDeployment{alias, candidate})
		}
	}
	s.mu.RUnlock()
	slices.SortFunc(deployments, func(a, b healthDeployment) int {
		if cmp := strings.Compare(a.alias, b.alias); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.candidate.DeploymentID, b.candidate.DeploymentID)
	})
	results := make([]bool, len(deployments))
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var workers sync.WaitGroup
	concurrency := min(8, len(deployments))
	for worker := range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := worker; index < len(deployments) && ctx.Err() == nil; index += concurrency {
				results[index] = s.probeDeployment(ctx, r, deployments[index].candidate)
			}
		}()
	}
	workers.Wait()
	healthy, unhealthy := make([]map[string]any, 0), make([]map[string]any, 0)
	for i, dep := range deployments {
		candidate := dep.candidate
		entry := map[string]any{"model": candidate.ModelID, "model_id": candidate.DeploymentID, "model_name": dep.alias}
		if admin {
			entry["api_base"] = candidate.APIBase
			entry["api_version"] = candidate.APIVersion
		}
		if results[i] {
			healthy = append(healthy, entry)
		} else {
			unhealthy = append(unhealthy, entry)
		}
	}
	status := http.StatusOK
	if (model != "" || id != "") && len(healthy) == 0 {
		status = http.StatusServiceUnavailable
	}
	if !admin {
		w.Header().Set("Litellm-Health-Field-Notice", "api_base and api_version are admin-only on this endpoint")
	}
	writeJSON(w, status, map[string]any{"healthy_endpoints": healthy, "unhealthy_endpoints": unhealthy,
		"healthy_count": len(healthy), "unhealthy_count": len(unhealthy)})
}

func (s *Server) probeDeployment(ctx context.Context, r *http.Request, candidate Candidate) bool {
	const prompt = "ping"
	endpoint := candidate.Endpoint
	var fields map[string]any
	switch {
	case isConversationEndpoint(endpoint):
		endpoint = "chat-completions"
		fields = map[string]any{"messages": []any{map[string]any{"role": "user", "content": prompt}}, "max_tokens": 5}
	case CompatibleEndpoints(endpoint, "embeddings"):
		endpoint = "embeddings"
		fields = map[string]any{"input": []string{prompt}}
	case endpoint == "completions":
		fields = map[string]any{"prompt": prompt, "max_tokens": 5}
	case endpoint == "moderations":
		fields = map[string]any{"input": prompt}
	case endpoint == "rerank":
		fields = map[string]any{"query": prompt, "documents": []string{prompt}, "top_n": 1}
	case endpoint == "speech":
		fields = map[string]any{"input": prompt, "voice": "alloy"}
	default:
		return false
	}
	status, _, _, _, err := s.callJSON(ctx, r, endpoint, candidate, requestPayload{fields: fields})
	return err == nil && status >= 200 && status < 300
}
