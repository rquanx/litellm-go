package runtime

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const anthropicBatchPath = "/v1/messages/batches"

func isAnthropicBatchPath(path string) bool {
	return path == anthropicBatchPath || strings.HasPrefix(path, anthropicBatchPath+"/")
}

func (s *Server) anthropicBatch(w http.ResponseWriter, request *http.Request, scope []string) {
	alias := request.URL.Query().Get("model")
	deployment := ""
	target := anthropicBatchPath
	var payload map[string]any
	var batchParams []map[string]any
	results := false
	if request.URL.Path == anthropicBatchPath && request.Method == http.MethodPost {
		if err := decodeBody(request, &payload, s.config.MaxBodyBytes); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Invalid Anthropic batch")
			return
		}
		items, ok := payload["requests"].([]any)
		if !ok || len(items) == 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "Supply batch requests")
			return
		}
		for _, item := range items {
			fields, _ := item.(map[string]any)
			params := objectField(fields, "params")
			model, _ := params["model"].(string)
			if model == "" || (alias != "" && alias != model) {
				writeError(w, http.StatusBadRequest, "invalid_request", "Batch items must use one configured model")
				return
			}
			alias = model
			batchParams = append(batchParams, params)
		}
	} else if request.URL.Path != anthropicBatchPath {
		raw := strings.TrimPrefix(request.URL.EscapedPath(), anthropicBatchPath+"/")
		id, action, _ := strings.Cut(raw, "/")
		decoded, err := url.PathUnescape(id)
		if err == nil {
			decoded, err = nativeResourceID(decoded, "anthropic_batches", &deployment)
		}
		results = action == "results" && request.Method == http.MethodGet
		if err != nil || !(results || action == "" && (request.Method == http.MethodGet || request.Method == http.MethodDelete) ||
			action == "cancel" && request.Method == http.MethodPost) {
			writeError(w, http.StatusNotFound, "batch_not_found", "Unknown Anthropic batch operation")
			return
		}
		target += "/" + url.PathEscape(decoded)
		if action != "" {
			target += "/" + action
		}
	} else if request.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Unsupported Anthropic batch method")
		return
	}
	for _, name := range []string{"before_id", "after_id"} {
		if cursor := request.URL.Query().Get(name); cursor != "" {
			if _, err := nativeResourceID(cursor, "anthropic_batches", &deployment); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_cursor", "Invalid Anthropic batch cursor")
				return
			}
		}
	}

	candidate, ok := s.anthropicBatchDeployment(alias, deployment, scope)
	if !ok {
		writeError(w, http.StatusBadRequest, "model_required", "Select one configured Anthropic deployment")
		return
	}
	for _, params := range batchParams {
		params["model"] = candidate.ModelID
	}
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
	}
	s.sendAnthropicBatch(w, request, candidate, target, body, results)
}

func (s *Server) anthropicBatchDeployment(alias, deployment string, scope []string) (Candidate, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var selected Candidate
	for name, route := range s.configuredRoutes {
		if (alias != "" && name != alias) || !modelAllowed(scope, name) {
			continue
		}
		for _, candidate := range route.Candidates {
			if candidate.Provider != "anthropic" || candidate.DeploymentID == "" || !candidateAllowed(scope, candidate) ||
				(deployment != "" && candidate.DeploymentID != deployment) {
				continue
			}
			if deployment != "" || alias != "" {
				return candidate, true
			}
			if selected.DeploymentID != "" && selected.DeploymentID != candidate.DeploymentID {
				return Candidate{}, false
			}
			selected = candidate
		}
	}
	return selected, selected.DeploymentID != ""
}

func (s *Server) sendAnthropicBatch(w http.ResponseWriter, request *http.Request, candidate Candidate, path string, body []byte, results bool) {
	base := strings.TrimSuffix(strings.TrimRight(candidate.APIBase, "/"), "/v1")
	upstream, err := http.NewRequestWithContext(request.Context(), request.Method, base+path, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Anthropic batch request")
		return
	}
	query := upstream.URL.Query()
	for _, field := range []string{"before_id", "after_id", "limit"} {
		if value := request.URL.Query().Get(field); value != "" {
			if field != "limit" {
				deployment := candidate.DeploymentID
				value, err = nativeResourceID(value, "anthropic_batches", &deployment)
				if err != nil {
					writeError(w, http.StatusBadRequest, "invalid_cursor", "Invalid Anthropic batch cursor")
					return
				}
			}
			query.Set(field, value)
		}
	}
	upstream.URL.RawQuery = query.Encode()
	upstream.Header.Set("anthropic-version", "2023-06-01")
	for _, name := range []string{"anthropic-beta", "anthropic-version"} {
		if values := request.Header.Values(name); len(values) != 0 {
			upstream.Header[http.CanonicalHeaderKey(name)] = values
		}
	}
	key := candidate.APIKey
	if key == "" && candidate.CredentialRef != "" {
		key = os.Getenv(candidate.CredentialRef)
	}
	upstream.Header.Set("x-api-key", key)
	upstream.Header.Set("Content-Type", "application/json")
	select {
	case s.sem <- struct{}{}:
	case <-request.Context().Done():
		return
	}
	defer func() { <-s.sem }()
	response, err := s.config.HTTPClient.Do(upstream)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not reach Anthropic batches")
		return
	}
	defer response.Body.Close()
	success := response.StatusCode >= 200 && response.StatusCode < 300
	if results && success {
		w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
		return
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes+1))
	if err != nil || int64(len(data)) > s.config.MaxBodyBytes {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not read Anthropic batch")
		return
	}
	if success {
		data = wrapOpenAIResource(data, "anthropic_batches", candidate.DeploymentID)
	}
	writeRawResponse(w, response.StatusCode, response.Header, data)
}
