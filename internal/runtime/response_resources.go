package runtime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const responseResourcePrefix = "/v1/responses/"

func (route *Route) pinResponseDeployment(provider, deployment string) bool {
	for _, candidate := range route.Candidates {
		if candidate.DeploymentID == deployment && candidate.Endpoint == "responses" &&
			(provider == "" || candidate.Provider == provider) {
			route.Candidates = []Candidate{candidate}
			route.ContextFallback = []Candidate{}
			route.PolicyFallback = []Candidate{}
			return true
		}
	}
	return false
}

func wrapResponseID(id string, candidate Candidate) string {
	managed := "litellm:custom_llm_provider:" + candidate.Provider + ";model_id:" + candidate.DeploymentID + ";response_id:" + id
	return "resp_" + base64.StdEncoding.EncodeToString([]byte(managed))
}

func unwrapResponseID(id string) (provider, deployment, original string, ok bool) {
	encoded, ok := strings.CutPrefix(id, "resp_")
	if !ok {
		return "", "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", "", false
	}
	parts := strings.SplitN(string(decoded), ";", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	provider, providerOK := strings.CutPrefix(parts[0], "litellm:custom_llm_provider:")
	deployment, deploymentOK := strings.CutPrefix(parts[1], "model_id:")
	original, originalOK := strings.CutPrefix(parts[2], "response_id:")
	return provider, deployment, original, providerOK && deploymentOK && originalOK && deployment != "" && original != ""
}

func wrapResponseBody(body []byte, candidate Candidate) []byte {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return body
	}
	if id, ok := payload["id"].(string); ok && id != "" {
		payload["id"] = wrapResponseID(id, candidate)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

func wrapResponseEvent(frame []byte, candidate Candidate) []byte {
	lines := bytes.Split(frame, []byte{'\n'})
	for index, line := range lines {
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		var event map[string]any
		if json.Unmarshal(bytes.TrimSpace(data), &event) != nil {
			continue
		}
		response, _ := event["response"].(map[string]any)
		if id, ok := response["id"].(string); ok && id != "" {
			response["id"] = wrapResponseID(id, candidate)
			encoded, _ := json.Marshal(event)
			lines[index] = append([]byte("data: "), encoded...)
		}
	}
	return bytes.Join(lines, []byte{'\n'})
}

func (s *Server) responseDeployment(id, provider string, scope []string) (Candidate, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for alias, route := range s.configuredRoutes {
		if !modelAllowed(scope, alias) {
			continue
		}
		for _, candidate := range route.Candidates {
			if candidateAllowed(scope, candidate) && candidate.DeploymentID == id && candidate.Provider == provider && candidate.Endpoint == "responses" {
				return candidate, true
			}
		}
	}
	return Candidate{}, false
}

func (s *Server) responseResource(writer http.ResponseWriter, request *http.Request, scope []string) {
	path := strings.TrimPrefix(request.URL.EscapedPath(), responseResourcePrefix)
	id, suffix, _ := strings.Cut(path, "/")
	decoded, err := url.PathUnescape(id)
	provider, deployment, original, ok := unwrapResponseID(decoded)
	if err != nil || !ok || original == "." || original == ".." || strings.ContainsAny(original, "/\\") {
		writeError(writer, http.StatusNotFound, "response_not_found", "Unknown response ID")
		return
	}
	method := request.Method
	if (suffix != "" || (method != http.MethodGet && method != http.MethodDelete)) &&
		!(suffix == "cancel" && method == http.MethodPost) && !(suffix == "input_items" && method == http.MethodGet) {
		writeError(writer, http.StatusNotFound, "response_not_found", "Unknown response operation")
		return
	}
	candidate, ok := s.responseDeployment(deployment, provider, scope)
	if !ok {
		writeError(writer, http.StatusNotFound, "response_not_found", "Unknown response deployment")
		return
	}
	resource := "/" + url.PathEscape(original)
	if suffix != "" {
		resource += "/" + suffix
	}
	base := strings.TrimSuffix(strings.TrimRight(candidate.APIBase, "/"), "/v1")
	target := base + "/v1/responses" + resource
	if candidate.Provider == "azure" {
		root, query, found := strings.Cut(azureTarget(base, candidate.ModelID, "responses", candidate.APIVersion), "?")
		target = root + resource
		if found {
			target += "?" + query
		}
	}
	upstream, err := http.NewRequestWithContext(request.Context(), method, target, nil)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "upstream_error", "Invalid upstream request")
		return
	}
	query := upstream.URL.Query()
	for _, key := range []string{"after", "before", "limit", "order", "include", "include[]"} {
		for _, value := range request.URL.Query()[key] {
			query.Add(key, value)
		}
	}
	upstream.URL.RawQuery = query.Encode()
	key := candidate.APIKey
	if key == "" && candidate.CredentialRef != "" {
		key = os.Getenv(candidate.CredentialRef)
	}
	if key != "" {
		if candidate.Provider == "azure" {
			upstream.Header.Set("api-key", key)
		} else {
			upstream.Header.Set("Authorization", "Bearer "+key)
		}
	}
	select {
	case s.sem <- struct{}{}:
	case <-request.Context().Done():
		return
	}
	defer func() { <-s.sem }()
	response, err := s.config.HTTPClient.Do(upstream)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "upstream_error", "Could not retrieve upstream response")
		return
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes+1))
	if err != nil || int64(len(body)) > s.config.MaxBodyBytes {
		writeError(writer, http.StatusBadGateway, "upstream_error", "Could not read upstream response")
		return
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 && suffix != "input_items" && len(bytes.TrimSpace(body)) != 0 {
		body = wrapResponseBody(body, candidate)
	}
	writeRawResponse(writer, response.StatusCode, response.Header, body)
}
