package runtime

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

const anthropicSkillPath = "/v1/skills"
const anthropicSkillFamily = "anthropic_skills"

func isAnthropicSkillPath(path string) bool {
	return path == anthropicSkillPath || strings.HasPrefix(path, anthropicSkillPath+"/")
}

func (s *Server) anthropicSkill(w http.ResponseWriter, r *http.Request, scope []string) {
	var deployment string
	path := r.URL.EscapedPath()
	query := r.URL.Query()
	alias := query.Get("model")
	if alias == "" {
		alias = r.Header.Get("X-LiteLLM-Model")
	}
	var body []byte
	var contentType string
	switch {
	case path == anthropicSkillPath && r.Method == http.MethodPost:
		raw, err := readBody(r, s.config.MaxBodyBytes)
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Anthropic skill upload is too large")
			return
		}
		form, err := resourceForm(r.Header.Get("Content-Type"), raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Expected a multipart skill upload")
			return
		}
		if model, _ := form.fields["model"].(string); model != "" {
			if alias != "" && model != alias {
				writeError(w, http.StatusBadRequest, "invalid_request", "Conflicting skill model selectors")
				return
			}
			alias = model
		}
		body, contentType, err = form.encodeForm("", false, true)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Invalid skill upload")
			return
		}
	case path == anthropicSkillPath && r.Method == http.MethodGet:
		if token := query.Get("page"); strings.HasPrefix(token, "minato_") {
			cursor, err := nativeResourceID(token, anthropicSkillFamily+"_page", &deployment)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_cursor", "Invalid Anthropic skill page")
				return
			}
			query.Set("page", cursor)
		}
	case path != anthropicSkillPath && (r.Method == http.MethodGet || r.Method == http.MethodDelete):
		id, err := decodeResourceSegment(strings.TrimPrefix(path, anthropicSkillPath+"/"), anthropicSkillFamily, &deployment)
		if err != nil {
			writeError(w, http.StatusNotFound, "resource_not_found", "Unknown Anthropic skill")
			return
		}
		path = anthropicSkillPath + "/" + url.PathEscape(id)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	candidate, ok := s.anthropicBatchDeployment(alias, deployment, scope)
	if !ok {
		writeError(w, http.StatusBadRequest, "model_required", "Select one configured Anthropic skill deployment")
		return
	}
	forward := url.Values{"beta": {"true"}}
	for _, field := range []string{"limit", "page", "source"} {
		if value := query.Get(field); value != "" {
			forward.Set(field, value)
		}
	}
	base := strings.TrimSuffix(strings.TrimRight(candidate.APIBase, "/"), "/v1")
	upstream, err := http.NewRequestWithContext(r.Context(), r.Method, base+path+"?"+forward.Encode(), bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Anthropic skill URL")
		return
	}
	key := candidate.APIKey
	if key == "" && candidate.CredentialRef != "" {
		key = os.Getenv(candidate.CredentialRef)
	}
	upstream.Header.Set("x-api-key", key)
	upstream.Header.Set("anthropic-version", "2023-06-01")
	upstream.Header.Set("anthropic-beta", "skills-2025-10-02")
	for _, field := range []string{"anthropic-version", "anthropic-beta"} {
		if value := r.Header.Get(field); value != "" {
			upstream.Header.Set(field, value)
		}
	}
	if contentType != "" {
		upstream.Header.Set("Content-Type", contentType)
	}
	select {
	case s.sem <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	defer func() { <-s.sem }()
	response, err := s.config.HTTPClient.Do(upstream)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not reach Anthropic skills")
		return
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes+1))
	if err != nil || int64(len(data)) > s.config.MaxBodyBytes {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not read Anthropic skill")
		return
	}
	if response.StatusCode/100 == 2 {
		data = wrapOpenAIResource(data, anthropicSkillFamily, candidate.DeploymentID)
	}
	writeRawResponse(w, response.StatusCode, response.Header, data)
}
