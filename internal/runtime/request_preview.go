package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

type previewContextKey struct{}

type rawRequestPreview struct {
	URL     string            `json:"raw_request_api_base"`
	Body    json.RawMessage   `json:"raw_request_body"`
	Headers map[string]string `json:"raw_request_headers"`
}

func (s *Server) transformPreview(w http.ResponseWriter, r *http.Request, scope []string) {
	var input struct {
		Call string         `json:"call_type"`
		Body map[string]any `json:"request_body"`
	}
	if err := decodeBody(r, &input, s.config.MaxBodyBytes); err != nil || input.Body == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Expected a call type and request body")
		return
	}
	call := strings.TrimPrefix(input.Call, "a")
	endpoint := map[string]string{"completion": "chat-completions", "responses": "responses", "embedding": "embeddings",
		"image_generation": "image-generation", "text_completion": "completions"}[call]
	if endpoint == "" {
		writeError(w, http.StatusBadRequest, "unsupported_endpoint", "This call type does not support request previews")
		return
	}
	alias, _ := input.Body["model"].(string)
	if !modelAllowed(scope, alias) {
		writeError(w, http.StatusForbidden, "model_not_allowed", "The key does not allow this model")
		return
	}
	s.mu.RLock()
	route, found := s.routes[alias]
	if !found {
		route, found = s.configuredRoutes[alias]
	}
	s.mu.RUnlock()
	if !found {
		writeError(w, http.StatusNotFound, "model_not_found", "Unknown runtime route")
		return
	}
	for _, name := range []string{"api_key", "api_base", "azure_ad_token", "extra_headers", "authorization", "router_settings_override", "metadata", "litellm_metadata"} {
		delete(input.Body, name)
	}
	stream := inputBool(input.Body, "stream")
	for _, candidate := range route.Candidates {
		if !candidateAllowed(scope, candidate) || !servesEndpoint(endpoint, candidate.Endpoint) {
			continue
		}
		if candidate.Endpoint == "bedrock-embeddings" || candidate.Endpoint == "google-embeddings" {
			continue
		}
		var captured rawRequestPreview
		ctx := context.WithValue(r.Context(), previewContextKey{}, &captured)
		_, err := s.send(ctx, r, endpoint, candidate, requestPayload{fields: input.Body}, stream)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Could not transform the request")
			return
		}
		writeJSON(w, http.StatusOK, captured)
		return
	}
	writeError(w, http.StatusBadRequest, "unsupported_endpoint", "Model has no compatible request preview")
}
