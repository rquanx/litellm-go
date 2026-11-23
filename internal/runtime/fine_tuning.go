package runtime

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

const fineTuningPath = "/v1/fine_tuning/jobs"
const fineTuningFamily = "fine_tuning_jobs"

func isFineTuningPath(path string) bool {
	return path == fineTuningPath || strings.HasPrefix(path, fineTuningPath+"/")
}

func (s *Server) fineTuningResource(w http.ResponseWriter, r *http.Request, scope []string) {
	path := r.URL.EscapedPath()
	query := r.URL.Query()
	alias, provider := query.Get("model"), query.Get("custom_llm_provider")
	if alias == "" {
		alias = r.Header.Get("X-LiteLLM-Model")
	}
	if path != fineTuningPath || r.Method != http.MethodPost {
		if s.vertexFineTuningResource(w, r, provider, alias, scope) {
			return
		}
	}
	var deployment string
	create := path == fineTuningPath && r.Method == http.MethodPost
	if path == fineTuningPath {
		if !create && r.Method != http.MethodGet {
			writeError(w, http.StatusNotFound, "resource_not_found", "Unknown fine-tuning operation")
			return
		}
	} else {
		parts := strings.Split(strings.TrimPrefix(path, fineTuningPath+"/"), "/")
		retrieve := len(parts) == 1 && r.Method == http.MethodGet
		cancel := len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost
		if !retrieve && !cancel {
			writeError(w, http.StatusNotFound, "resource_not_found", "Unknown fine-tuning operation")
			return
		}
		id, err := decodeResourceSegment(parts[0], fineTuningFamily, &deployment)
		if err != nil {
			writeError(w, http.StatusNotFound, "resource_not_found", "Invalid fine-tuning job ID")
			return
		}
		path = fineTuningPath + "/" + url.PathEscape(id)
		if cancel {
			path += "/cancel"
		}
	}
	for _, id := range query["after"] {
		if _, err := nativeResourceID(id, fineTuningFamily, &deployment); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "Invalid fine-tuning cursor")
			return
		}
	}
	var fields map[string]any
	if r.Method == http.MethodPost {
		body, err := readBody(r, s.config.MaxBodyBytes)
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Fine-tuning request body is too large")
			return
		}
		if len(body) > 0 {
			payload, err := decodeJSONPayload(body)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", "Expected a fine-tuning JSON request")
				return
			}
			fields = payload.fields
			if value, _ := fields["custom_llm_provider"].(string); value != "" {
				provider = value
			}
			delete(fields, "custom_llm_provider")
		}
	}
	if create {
		model, _ := fields["model"].(string)
		training, _ := fields["training_file"].(string)
		if model == "" || training == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "A model and training file are required")
			return
		}
		if alias == "" {
			alias = model
		}
		if provider == "vertex_ai" || (provider == "" && strings.HasPrefix(training, "gs://")) {
			s.createVertexFineTuning(w, r, alias, scope, fields)
			return
		}
		for _, name := range []string{"training_file", "validation_file"} {
			if id, _ := fields[name].(string); id != "" {
				if isLegacyManagedFile(id) {
					candidate, original, ok := s.resolveLegacyFile(r, id, alias, scope)
					if !ok || (deployment != "" && deployment != candidate.DeploymentID) {
						writeError(w, http.StatusBadRequest, "invalid_request", "Managed file is unavailable on this deployment")
						return
					}
					deployment, id = candidate.DeploymentID, original
				} else if fileAlias, original, ok := decodeLiteLLMResourceID(id, "files"); ok {
					if alias != fileAlias {
						writeError(w, http.StatusBadRequest, "invalid_request", "Fine-tuning file belongs to a different model")
						return
					}
					id = original
				}
				native, err := nativeResourceID(id, "files", &deployment)
				if err != nil {
					writeError(w, http.StatusBadRequest, "invalid_request", "Invalid or mismatched fine-tuning file ID")
					return
				}
				fields[name] = native
			}
		}
	}
	candidate, ok := s.selectResourceCandidate(alias, deployment, provider, scope)
	if !ok {
		writeError(w, http.StatusBadRequest, "model_required", "Select one configured OpenAI or Azure fine-tuning deployment")
		return
	}
	var body []byte
	var contentType string
	if create {
		fields["model"] = candidate.ModelID
		body, _ = json.Marshal(fields)
		contentType = "application/json"
	}
	s.forwardOpenAIResource(w, r, candidate, fineTuningFamily, path, contentType, body)
}
