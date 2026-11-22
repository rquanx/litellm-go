package runtime

import (
	"encoding/json"
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
			decoded, err = nativeResourceID(decoded, family, &deployment)
		}
		if err != nil ||
			!(request.Method == http.MethodGet && action == "" ||
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
