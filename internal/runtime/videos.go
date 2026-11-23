package runtime

import (
	"encoding/json"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
)

const videoPath = "/v1/videos"

func isVideoPath(path string) bool {
	return path == videoPath || strings.HasPrefix(path, videoPath+"/")
}

func (s *Server) videoResource(w http.ResponseWriter, request *http.Request, scope []string) {
	family := "videos"
	target := videoPath
	var deployment string
	var body []byte
	contentType := request.Header.Get("Content-Type")
	alias := request.URL.Query().Get("model")
	provider := request.URL.Query().Get("custom_llm_provider")
	path := strings.TrimPrefix(request.URL.EscapedPath(), videoPath)
	switch path {
	case "":
		if request.Method != http.MethodGet && request.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Unsupported video method")
			return
		}
	case "/edits", "/extensions", "/characters":
		if request.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Unsupported video method")
			return
		}
		target += path
		if path == "/characters" {
			family = "video_characters"
		}
	default:
		prefix := ""
		raw := strings.TrimPrefix(path, "/")
		if strings.HasPrefix(raw, "characters/") {
			prefix = "/characters"
			raw = strings.TrimPrefix(raw, "characters/")
			family = "video_characters"
		}
		id, action, _ := strings.Cut(raw, "/")
		decoded, err := url.PathUnescape(id)
		if err == nil {
			if oldProvider, model, original, managed := decodeLiteLLMVideoID(decoded, family); managed {
				if provider != "" && provider != oldProvider {
					writeError(w, http.StatusBadRequest, "invalid_request", "Video ID belongs to a different provider")
					return
				}
				provider = oldProvider
				if model != "" {
					resolved, ok := s.videoDeploymentForModel(alias, model, provider, scope)
					if !ok || (deployment != "" && deployment != resolved) {
						writeError(w, http.StatusBadRequest, "model_required", "Video ID requires a matching configured deployment")
						return
					}
					deployment = resolved
				}
				decoded = original
				if provider == "gemini" || provider == "vertex_ai" {
					decoded = base64.RawURLEncoding.EncodeToString([]byte(original))
				}
			}
			decoded, err = nativeResourceID(decoded, family, &deployment)
		}
		if err != nil ||
			!((request.Method == http.MethodGet || request.Method == http.MethodDelete) && action == "" ||
				family == "videos" && (request.Method == http.MethodGet && action == "content" ||
					request.Method == http.MethodPost && action == "remix")) {
			writeError(w, http.StatusNotFound, "video_not_found", "Unknown video resource operation")
			return
		}
		target += prefix + "/" + url.PathEscape(decoded)
		if action != "" {
			target += "/" + action
		}
	}
	if cursor := request.URL.Query().Get("after"); cursor != "" {
		if _, err := nativeResourceID(cursor, family, &deployment); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "Invalid video cursor")
			return
		}
	}
	var payload requestPayload
	if request.Method == http.MethodPost {
		raw, err := readBody(request, s.config.MaxBodyBytes)
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large")
			return
		}
		if strings.HasPrefix(contentType, "multipart/form-data") {
			payload, err = resourceForm(contentType, raw)
		} else {
			payload, err = decodeJSONPayload(raw)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Invalid video request body")
			return
		}
		if requested, _ := payload.fields["custom_llm_provider"].(string); requested != "" {
			provider = requested
		}
		delete(payload.fields, "custom_llm_provider")
		if model, _ := payload.fields["model"].(string); model != "" {
			if alias != "" && alias != model {
				writeError(w, http.StatusBadRequest, "invalid_request", "Conflicting video models")
				return
			}
			alias = model
		}
	}
	if provider == "runwayml" || (provider == "" && (alias != "" || deployment != "")) {
		if candidate, ok := s.selectRunwayVideo(alias, deployment, scope); ok {
			s.runwayVideoResource(w, request, candidate, target, payload)
			return
		}
	}
	if request.Method == http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Unsupported video operation")
		return
	}
	candidate, ok := s.selectResourceCandidate(alias, deployment, provider, scope)
	if !ok && ((provider == "" && (alias != "" || deployment != "")) || provider == "gemini" || provider == "vertex_ai") {
		if veo, found := s.selectGoogleResourceCandidate(alias, deployment, provider, scope, true); found {
			s.googleVideoResource(w, request, veo, target, payload)
			return
		}
	}
	if !ok {
		writeError(w, http.StatusBadRequest, "model_required", "Select one configured video deployment")
		return
	}
	if request.Method == http.MethodPost {
		if payload.form != nil {
			var err error
			body, contentType, err = payload.encodeMultipart(candidate.ModelID, false)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", "Invalid multipart video data")
				return
			}
		} else {
			if alias != "" || path == "" {
				payload.fields["model"] = candidate.ModelID
			}
			body, _ = json.Marshal(payload.fields)
			contentType = "application/json"
		}
	}
	s.forwardOpenAIResource(w, request, candidate, family, target, contentType, body)
}

func decodeLiteLLMVideoID(id, family string) (provider, model, original string, ok bool) {
	prefix := "video_"
	label := "video_id:"
	if family == "video_characters" {
		prefix, label = "character_", "character_id:"
	}
	encoded, ok := strings.CutPrefix(id, prefix)
	if !ok || len(encoded) > 8192 {
		return "", "", "", false
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(encoded, "="))
	if err != nil {
		return "", "", "", false
	}
	parts := strings.SplitN(string(raw), ";", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	provider, hasProvider := strings.CutPrefix(parts[0], "litellm:custom_llm_provider:")
	model, hasModel := strings.CutPrefix(parts[1], "model_id:")
	original, hasID := strings.CutPrefix(parts[2], label)
	return provider, model, original, hasProvider && hasModel && hasID && provider != "" && original != ""
}

func (s *Server) videoDeploymentForModel(alias, model, provider string, scope []string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var deployment string
	for name, route := range s.configuredRoutes {
		if (alias != "" && name != alias) || !modelAllowed(scope, name) {
			continue
		}
		for _, candidate := range route.Candidates {
			if candidate.Provider != provider || !candidateAllowed(scope, candidate) || candidate.DeploymentID == "" ||
				(model != name && model != candidate.ModelID && model != candidate.DeploymentID) {
				continue
			}
			if deployment != "" && deployment != candidate.DeploymentID {
				return "", false
			}
			deployment = candidate.DeploymentID
		}
	}
	return deployment, deployment != ""
}
