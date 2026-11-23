package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"time"
)

type healthDeployment struct {
	alias     string
	candidate Candidate
}

func (s *Server) modelHealth(w http.ResponseWriter, r *http.Request, scope []string, admin bool) {
	query := r.URL.Query()
	model, id := query.Get("model"), query.Get("model_id")
	background := s.config.BackgroundHealthInterval > 0
	deployments := s.healthDeployments(model, id, scope, background || s.config.SkipDisabledHealthChecks)
	var results []ModelHealthRecord
	if background {
		s.mu.RLock()
		cache := s.healthResults
		s.mu.RUnlock()
		checked := deployments[:0]
		for _, dep := range deployments {
			if entry, exists := cache[dep.candidate.DeploymentID]; exists {
				checked = append(checked, dep)
				results = append(results, entry)
			}
		}
		deployments = checked
	} else {
		results = s.runHealthChecks(r, deployments)
	}
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

func (s *Server) healthDeployments(model, id string, scope []string, skipDisabled bool) []healthDeployment {
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
				(skipDisabled && candidate.DisableBackgroundHealthCheck) ||
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
	return deployments
}

func (s *Server) runHealthChecks(r *http.Request, deployments []healthDeployment) []ModelHealthRecord {
	results := make([]ModelHealthRecord, len(deployments))
	ctx := r.Context()
	var workers sync.WaitGroup
	concurrency := min(s.config.HealthCheckConcurrency, len(deployments))
	for worker := range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := worker; index < len(deployments) && ctx.Err() == nil; index += concurrency {
				deployment := deployments[index]
				start := time.Now()
				entry := ModelHealthRecord{ID: newUUID(), Model: deployment.alias, ModelID: deployment.candidate.DeploymentID,
					Status: "unhealthy", UnhealthyCount: 1, CheckedAt: start, CreatedAt: start}
				timeout := deployment.candidate.HealthTimeout
				if timeout <= 0 {
					timeout = 10 * time.Second
				}
				probeCtx, cancel := context.WithTimeout(ctx, timeout)
				entry.responseStatus = s.probeDeployment(probeCtx, r, deployment.candidate)
				cancel()
				if entry.responseStatus >= 200 && entry.responseStatus < 300 {
					entry.Status, entry.HealthyCount, entry.UnhealthyCount = "healthy", 1, 0
				}
				entry.ResponseMS = float64(time.Since(start)) / float64(time.Millisecond)
				results[index] = entry
			}
		}()
	}
	workers.Wait()
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
	return results
}

func (s *Server) RunBackgroundHealthChecks(ctx context.Context) {
	interval := s.config.BackgroundHealthInterval
	if interval <= 0 {
		return
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/health", nil)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		s.mu.RLock()
		generation := s.configGeneration
		s.mu.RUnlock()
		deployments := s.healthDeployments("", "", nil, true)
		results := s.runHealthChecks(request, deployments)
		cache := make(map[string]ModelHealthRecord, len(results))
		for _, entry := range results {
			if entry.ID != "" {
				cache[entry.ModelID] = entry
			}
		}
		s.mu.Lock()
		if s.configGeneration == generation {
			s.healthResults = cache
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) filterUnhealthyCandidates(candidates []Candidate) []Candidate {
	staleness := s.config.HealthRoutingStaleness
	if staleness <= 0 || len(candidates) < 2 {
		return candidates
	}
	s.mu.RLock()
	cache := s.healthResults
	s.mu.RUnlock()
	if len(cache) == 0 {
		return candidates
	}
	cutoff := time.Now().Add(-staleness)
	filtered := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		result := cache[candidate.DeploymentID]
		transient := s.config.IgnoreTransientHealthErrors &&
			(result.responseStatus == http.StatusTooManyRequests || result.responseStatus == http.StatusRequestTimeout)
		if result.Status != "unhealthy" || !result.CheckedAt.After(cutoff) || transient {
			filtered = append(filtered, candidate)
		}
	}
	if len(filtered) == 0 {
		return candidates
	}
	return filtered
}

func (s *Server) probeDeployment(ctx context.Context, r *http.Request, candidate Candidate) int {
	if candidate.HealthModel != "" {
		candidate.ModelID = candidate.HealthModel
	}
	if candidate.HealthMode == "realtime" {
		return s.probeRealtime(ctx, candidate)
	}
	if candidate.HealthMode == "batch" {
		switch candidate.Provider {
		case "openai", "azure", "hosted_vllm", "vertex_ai":
			return s.probeBatchList(ctx, r, candidate)
		}
	}
	if candidate.HealthMode == "video_generation" || candidate.Endpoint == "videos" {
		return s.probeVideo(ctx, r, candidate)
	}
	const prompt = "ping"
	endpoint := candidate.Endpoint
	var fields map[string]any
	var form []formPart
	switch {
	case endpoint == "responses":
		fields = map[string]any{"input": prompt}
	case isConversationEndpoint(endpoint):
		endpoint = "chat-completions"
		fields = map[string]any{"messages": []any{map[string]any{"role": "user", "content": prompt}}}
	case CompatibleEndpoints(endpoint, "embeddings"):
		endpoint = "embeddings"
		fields = map[string]any{"input": []string{prompt}}
	case endpoint == "completions":
		fields = map[string]any{"prompt": prompt}
	case endpoint == "moderations":
		fields = map[string]any{"input": prompt}
	case endpoint == "rerank":
		fields = map[string]any{"query": prompt, "documents": []string{prompt}, "top_n": 1}
	case endpoint == "speech":
		voice := candidate.HealthVoice
		if voice == "" {
			voice = "alloy"
		}
		fields = map[string]any{"input": prompt, "voice": voice}
	case endpoint == "image-generation":
		fields = map[string]any{"prompt": prompt}
	case endpoint == "transcription":
		form = []formPart{{
			name: "file",
			header: textproto.MIMEHeader{
				"Content-Disposition": {`form-data; name="file"; filename="audio_health_check.wav"`},
				"Content-Type":        {"audio/wav"},
			},
			body: healthProbeWAV,
		}, {
			name: "model",
			header: textproto.MIMEHeader{
				"Content-Disposition": {`form-data; name="model"`},
			},
		}}
	case endpoint == "ocr":
		fields = map[string]any{"document": map[string]any{"type": "document_url", "document_url": healthProbePDF}}
	default:
		return http.StatusNotImplemented
	}
	if fields != nil {
		applyHealthGenerationOptions(fields, candidate, endpoint)
	}
	status, _, _, _, err := s.callJSON(ctx, r, endpoint, candidate, requestPayload{fields: fields, form: form})
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return http.StatusRequestTimeout
		}
		return http.StatusBadGateway
	}
	return status
}

// A Realtime health check only opens the provider WebSocket.
func (s *Server) probeRealtime(ctx context.Context, candidate Candidate) int {
	switch candidate.Provider {
	case "openai", "azure", "xai":
	default:
		return http.StatusNotImplemented
	}

	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return http.StatusRequestTimeout
	}
	defer func() { <-s.sem }()

	conn, err := s.dialProviderSocket(ctx, candidate, realtimeAddress(candidate, ""), make(http.Header))
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return http.StatusRequestTimeout
		}
		return http.StatusBadGateway
	}
	_ = conn.CloseNow()
	return http.StatusOK
}
func (s *Server) probeBatchList(ctx context.Context, parent *http.Request, candidate Candidate) int {
	request := ragSubrequest(parent.WithContext(ctx), http.MethodGet, "/v1/batches?limit=1", nil, false)
	result := &ragResult{header: make(http.Header), limit: s.config.MaxBodyBytes}
	if candidate.Provider == "vertex_ai" {
		if candidate.Vertex == nil {
			return http.StatusBadGateway
		}
		if _, ok := s.sendVertexBatch(result, request, candidate, http.MethodGet, vertexBatchPath(candidate)+"?pageSize=1", nil); ok {
			return http.StatusOK
		}
	} else {
		s.forwardOpenAIResource(result, request, candidate, "", "/v1/batches", "", nil)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return http.StatusRequestTimeout
	}
	return result.status
}

func (s *Server) probeVideo(ctx context.Context, parent *http.Request, candidate Candidate) int {
	fields := map[string]any{"prompt": "test video generation", "model": candidate.ModelID}
	result := &ragResult{header: make(http.Header), limit: s.config.MaxBodyBytes}
	request := ragSubrequest(parent.WithContext(ctx), http.MethodPost, videoPath, nil, false)
	switch candidate.Provider {
	case "openai", "azure":
		body, _ := json.Marshal(fields)
		s.forwardOpenAIResource(result, request, candidate, "", videoPath, "application/json", body)
	case "gemini", "vertex_ai":
		s.googleVideoResource(result, request, candidate, videoPath, requestPayload{fields: fields})
	default:
		return http.StatusNotImplemented
	}
	if ctx.Err() == context.DeadlineExceeded {
		return http.StatusRequestTimeout
	}
	return result.status
}

func applyHealthGenerationOptions(fields map[string]any, candidate Candidate, endpoint string) {
	chat := endpoint == "chat-completions"
	completion := endpoint == "completions"
	responses := endpoint == "responses"
	supportsMaxTokens := chat || completion || responses
	if candidate.HealthSupportsMaxTokens != nil {
		supportsMaxTokens = *candidate.HealthSupportsMaxTokens
	}
	if supportsMaxTokens {
		field := "max_tokens"
		if responses {
			field = "max_output_tokens"
		}
		if candidate.HealthMaxTokens != nil {
			fields[field] = *candidate.HealthMaxTokens
		} else if (chat || completion) && !strings.Contains(candidate.ModelID, "*") {
			fields[field] = 5
		}
	}
	if (chat || completion) && candidate.HealthReasoningEffort != "" {
		fields["reasoning_effort"] = candidate.HealthReasoningEffort
	}
}
