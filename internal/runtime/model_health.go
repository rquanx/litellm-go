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
	results := make([]ModelHealthRecord, len(deployments))
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var workers sync.WaitGroup
	concurrency := min(8, len(deployments))
	for worker := range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := worker; index < len(deployments) && ctx.Err() == nil; index += concurrency {
				deployment := deployments[index]
				start := time.Now()
				entry := ModelHealthRecord{ID: newUUID(), Model: deployment.alias, ModelID: deployment.candidate.DeploymentID,
					Status: "unhealthy", UnhealthyCount: 1, CheckedAt: start, CreatedAt: start}
				if s.probeDeployment(ctx, r, deployment.candidate) {
					entry.Status, entry.HealthyCount, entry.UnhealthyCount = "healthy", 1, 0
				}
				entry.ResponseMS = float64(time.Since(start)) / float64(time.Millisecond)
				results[index] = entry
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
		if results[i].HealthyCount != 0 {
			healthy = append(healthy, entry)
		} else {
			unhealthy = append(unhealthy, entry)
		}
	}
	if s.config.HealthHistory != nil {
		saved := make([]ModelHealthRecord, 0, len(results))
		for _, result := range results {
			if result.ID != "" {
				saved = append(saved, result)
			}
		}
		storeCtx, cancelStore := context.WithTimeout(r.Context(), time.Second)
		defer cancelStore()
		s.config.HealthHistory.Record(storeCtx, saved)
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
