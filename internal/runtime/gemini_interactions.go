package runtime

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
)

const interactionPath = "/v1beta/interactions"
const interactionFamily = "gemini_interactions"

func interactionSuffix(path string) (string, bool) {
	for _, base := range []string{interactionPath, "/interactions"} {
		if path == base || strings.HasPrefix(path, base+"/") {
			return strings.TrimPrefix(path, base), true
		}
	}
	return "", false
}

func (s *Server) geminiResourceCandidate(alias, deployment string, scope []string) (Candidate, bool) {
	return s.selectGoogleResourceCandidate(alias, deployment, "gemini", scope, false)
}

func (s *Server) selectGoogleResourceCandidate(alias, deployment, provider string, scope []string, video bool) (Candidate, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var selected Candidate
	for name, route := range s.configuredRoutes {
		if !modelAllowed(scope, name) || (alias != "" && name != alias) {
			continue
		}
		for _, candidate := range route.Candidates {
			google := slices.Contains([]string{"gemini", "vertex_ai"}, candidate.Provider)
			compatible := isGeminiEndpoint(candidate.Endpoint)
			if video {
				compatible = strings.HasPrefix(candidate.ModelID, "veo-")
			}
			if !google || (provider != "" && provider != candidate.Provider) || !compatible || candidate.DeploymentID == "" ||
				!candidateAllowed(scope, candidate) {
				continue
			}
			if alias != "" && candidate.RoutingGroup != "" && candidate.RoutingGroup != alias && deployment == "" {
				continue
			}
			if deployment != "" {
				if candidate.DeploymentID == deployment {
					return candidate, true
				}
				continue
			}
			if alias != "" {
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

func (s *Server) serveInteraction(w http.ResponseWriter, r *http.Request, suffix string, scope []string) {
	var deployment, alias, nativeID string
	var fields map[string]any
	var stream, agent bool
	targetPath := interactionPath
	if suffix == "" && r.Method == http.MethodPost {
		body, err := readBody(r, s.config.MaxBodyBytes)
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Interaction request body is too large")
			return
		}
		payload, err := decodeJSONPayload(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Expected an interaction JSON request")
			return
		}
		fields = payload.fields
		alias, _ = fields["model"].(string)
		if alias == "" {
			alias, _ = fields["agent"].(string)
			agent = alias != ""
		}
		if alias == "" || (fields["custom_llm_provider"] != nil && fields["custom_llm_provider"] != "gemini") {
			writeError(w, http.StatusBadRequest, "invalid_request", "A Gemini model or agent is required")
			return
		}
		if previous, _ := fields["previous_interaction_id"].(string); previous != "" {
			id, err := nativeResourceID(previous, interactionFamily, &deployment)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", "Invalid previous interaction ID")
				return
			}
			fields["previous_interaction_id"] = id
		}
		stream = inputBool(fields, "stream")
	} else {
		parts := strings.Split(strings.TrimPrefix(suffix, "/"), "/")
		read := len(parts) == 1 && (r.Method == http.MethodGet || r.Method == http.MethodDelete)
		cancel := len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost
		if !read && !cancel {
			writeError(w, http.StatusNotFound, "resource_not_found", "Unknown interaction operation")
			return
		}
		id, err := decodeResourceSegment(parts[0], interactionFamily, &deployment)
		if err != nil {
			writeError(w, http.StatusNotFound, "resource_not_found", "Invalid interaction ID")
			return
		}
		nativeID = id
		targetPath += "/" + url.PathEscape(id)
		if cancel {
			targetPath += ":cancel"
		}
		alias = r.URL.Query().Get("model")
	}
	candidate, ok := s.geminiResourceCandidate(alias, deployment, scope)
	if !ok {
		writeError(w, http.StatusBadRequest, "model_required", "Select a configured Gemini interaction deployment")
		return
	}
	var body []byte
	if fields != nil {
		payload := make(map[string]any)
		for _, key := range []string{"input", "tools", "system_instruction", "generation_config", "stream", "store",
			"background", "response_modalities", "response_format", "response_mime_type", "previous_interaction_id"} {
			if fields[key] != nil {
				payload[key] = fields[key]
			}
		}
		selector := "model"
		if agent {
			selector = "agent"
		}
		payload[selector] = strings.TrimPrefix(candidate.ModelID, "models/")
		body, _ = json.Marshal(payload)
	}
	base := strings.TrimSuffix(strings.TrimRight(candidate.APIBase, "/"), "/v1beta")
	target := base + targetPath
	if stream {
		target += "?alt=sse"
	}
	if s.config.Callback != nil && fields != nil {
		event := newInferenceEvent("interactions", alias, stream)
		if key := ProxyCredential(r); key != "" && !s.authorized(r) {
			event.KeyDigest = fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
		}
		if s.config.LogMessages {
			event.Messages = boundedCallbackJSON(fields["input"])
		}
		recorder := &callbackResponse{ResponseWriter: w, event: event, capture: s.config.LogMessages}
		writeRuntimeMeta(recorder, candidate, 1, nil)
		defer func() {
			if recorder.status != 0 {
				recorder.finish()
				s.config.Callback(recorder.event)
			}
		}()
		w = recorder
	}
	s.forwardInteraction(w, r, candidate, alias, nativeID, target, body, stream)
}

func (s *Server) forwardInteraction(w http.ResponseWriter, r *http.Request, candidate Candidate, alias, nativeID, target string, body []byte, stream bool) {
	upstream, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid interaction provider URL")
		return
	}
	key := candidate.APIKey
	if key == "" && candidate.CredentialRef != "" {
		key = os.Getenv(candidate.CredentialRef)
	}
	upstream.Header.Set("Content-Type", "application/json")
	if err := applyGeminiCredential(upstream, candidate, key); err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	for name, value := range candidate.Headers {
		upstream.Header.Set(name, value)
	}
	select {
	case s.sem <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	defer func() { <-s.sem }()
	response, err := s.config.HTTPClient.Do(upstream)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not reach the interaction provider")
		return
	}
	defer response.Body.Close()
	success := response.StatusCode >= 200 && response.StatusCode < 300
	if success && stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(response.StatusCode)
		flush, _ := w.(http.Flusher)
		reader := bufio.NewReader(response.Body)
		for {
			frame, err := readSSEFrame(reader, s.config.MaxBodyBytes)
			if !writeStreamFrame(w, flush, wrapInteractionEvent(frame, candidate.DeploymentID, alias)) || err != nil {
				return
			}
		}
	}
	body, err = io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes+1))
	if err != nil || int64(len(body)) > s.config.MaxBodyBytes {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not read the interaction response")
		return
	}
	if success && r.Method == http.MethodDelete {
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "id": encodeResourceID(interactionFamily, candidate.DeploymentID, nativeID)})
		return
	}
	if success {
		body = wrapInteractionJSON(body, candidate.DeploymentID, alias, r.Method != http.MethodPost || nativeID == "")
	}
	writeRawResponse(w, response.StatusCode, response.Header, body)
}

func wrapInteractionJSON(body []byte, deployment, alias string, addObject bool) []byte {
	var event map[string]any
	if json.Unmarshal(body, &event) != nil {
		return body
	}
	interaction, _ := event["interaction"].(map[string]any)
	changed := false
	if addObject && event != nil && event["object"] == nil {
		event["object"] = "interaction"
		changed = true
	}
	for _, object := range []map[string]any{event, interaction} {
		if model, _ := object["model"].(string); model != "" && alias != "" && model != alias {
			object["model"] = alias
			changed = true
		}
		if id, _ := object["id"].(string); validResourceSegment(id) {
			object["id"] = encodeResourceID(interactionFamily, deployment, id)
			changed = true
		}
	}
	if !changed {
		return body
	}
	data, _ := json.Marshal(event)
	return data
}

func wrapInteractionEvent(frame []byte, deployment, alias string) []byte {
	lines := bytes.Split(frame, []byte{'\n'})
	for i, line := range lines {
		if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			lines[i] = append([]byte("data: "), wrapInteractionJSON(bytes.TrimSpace(data), deployment, alias, true)...)
		}
	}
	return bytes.Join(lines, []byte{'\n'})
}
