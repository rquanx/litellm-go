package runtime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func resourceFamily(path string) string {
	for _, family := range []string{"files", "batches"} {
		base := "/v1/" + family
		if path == base || strings.HasPrefix(path, base+"/") {
			return family
		}
	}
	return ""
}

type openAIResourceID struct {
	Family     string `json:"family"`
	Deployment string `json:"deployment"`
	Original   string `json:"original"`
}

func encodeResourceID(family, deployment, id string) string {
	data, _ := json.Marshal(openAIResourceID{Family: family, Deployment: deployment, Original: id})
	return "minato_" + base64.RawURLEncoding.EncodeToString(data)
}

func decodeResourceID(id, family string) (openAIResourceID, bool) {
	var result openAIResourceID
	encoded, ok := strings.CutPrefix(id, "minato_")
	if !ok || len(encoded) > 8192 {
		return result, false
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || json.Unmarshal(data, &result) != nil || result.Family != family ||
		!validResourceSegment(result.Original) || result.Deployment == "" {
		return openAIResourceID{}, false
	}
	return result, true
}

func validResourceSegment(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\")
}

func (s *Server) resourceCandidate(alias, deployment string, scope []string) (Candidate, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var selected Candidate
	for name, route := range s.configuredRoutes {
		if !modelAllowed(scope, name) || (alias != "" && name != alias) {
			continue
		}
		for _, candidate := range route.Candidates {
			if !candidateAllowed(scope, candidate) || (candidate.Provider != "openai" && candidate.Provider != "azure") || candidate.DeploymentID == "" {
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

func (s *Server) openAIResource(writer http.ResponseWriter, request *http.Request, scope []string) {
	family := resourceFamily(request.URL.Path)
	base := "/v1/" + family
	method := request.Method
	alias := request.URL.Query().Get("model")
	if alias == "" {
		alias = strings.TrimSpace(request.URL.Query().Get("target_model_names"))
	}
	var deployment string
	if cursor, ok := decodeResourceID(request.URL.Query().Get("after"), family); ok {
		deployment = cursor.Deployment
	}
	var body []byte
	contentType := request.Header.Get("Content-Type")
	targetPath := base
	if request.URL.Path == base {
		switch method {
		case http.MethodGet:
		case http.MethodPost:
			incoming, err := readBody(request, s.config.MaxBodyBytes)
			if err != nil {
				writeError(writer, http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large")
				return
			}
			if family == "files" {
				form, err := resourceForm(contentType, incoming)
				if err != nil {
					writeError(writer, http.StatusBadRequest, "invalid_request", "Expected a multipart file upload")
					return
				}
				if alias == "" {
					alias, _ = form.fields["target_model_names"].(string)
				}
				if alias == "" {
					alias, _ = form.fields["model"].(string)
				}
				if storage := form.fields["target_storage"]; storage != nil && storage != "" && storage != "default" {
					writeError(writer, http.StatusBadRequest, "invalid_request", "Unsupported file storage")
					return
				}
				body, contentType, err = form.encodeForm("", false, true)
			} else {
				var payload requestPayload
				payload, err = decodeJSONPayload(incoming)
				if err == nil {
					if alias == "" {
						alias, _ = payload.fields["model"].(string)
					}
					fileID, _ := payload.fields["input_file_id"].(string)
					if file, ok := decodeResourceID(fileID, "files"); ok {
						deployment = file.Deployment
						payload.fields["input_file_id"] = file.Original
					} else if strings.HasPrefix(fileID, "minato_") {
						err = errors.New("invalid input file ID")
					}
					delete(payload.fields, "model")
					delete(payload.fields, "target_model_names")
					if err == nil {
						body, err = json.Marshal(payload.fields)
					}
					contentType = "application/json"
				}
			}
			if err != nil {
				writeError(writer, http.StatusBadRequest, "invalid_request", "Invalid resource creation request")
				return
			}
		default:
			writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "Unsupported resource method")
			return
		}
	} else {
		path := strings.TrimPrefix(request.URL.EscapedPath(), base+"/")
		id, action, _ := strings.Cut(path, "/")
		decoded, err := url.PathUnescape(id)
		if err != nil {
			decoded = ""
		}
		if resource, ok := decodeResourceID(decoded, family); ok {
			deployment = resource.Deployment
			decoded = resource.Original
		} else if strings.HasPrefix(decoded, "minato_") {
			decoded = ""
		}
		if !validResourceSegment(decoded) || !resourceActionAllowed(family, action, method) {
			writeError(writer, http.StatusNotFound, "resource_not_found", "Unknown resource operation")
			return
		}
		targetPath += "/" + url.PathEscape(decoded)
		if action != "" {
			targetPath += "/" + action
		}
	}
	candidate, ok := s.resourceCandidate(alias, deployment, scope)
	if !ok {
		writeError(writer, http.StatusBadRequest, "model_required", "Select one configured resource deployment")
		return
	}
	s.forwardOpenAIResource(writer, request, candidate, family, targetPath, contentType, body)
}

func resourceForm(contentType string, body []byte) (requestPayload, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return requestPayload{}, errors.New("expected multipart/form-data")
	}
	return readMultipart(body, params["boundary"])
}

func resourceActionAllowed(family, action, method string) bool {
	switch action {
	case "":
		return method == http.MethodGet || (family == "files" && method == http.MethodDelete)
	case "content":
		return family == "files" && method == http.MethodGet
	case "cancel":
		return family == "batches" && method == http.MethodPost
	}
	return false
}

func (s *Server) forwardOpenAIResource(writer http.ResponseWriter, request *http.Request, candidate Candidate,
	family, path, contentType string, body []byte) {
	base := strings.TrimRight(candidate.APIBase, "/")
	target := strings.TrimSuffix(base, "/v1") + path
	if candidate.Provider == "azure" && !strings.HasSuffix(base, "/openai/v1") {
		target = strings.TrimSuffix(base, "/openai") + "/openai" + strings.TrimPrefix(path, "/v1") + "?api-version=" + url.QueryEscape(candidate.APIVersion)
	}
	upstream, err := http.NewRequestWithContext(request.Context(), request.Method, target, bytes.NewReader(body))
	if err != nil {
		writeError(writer, http.StatusBadGateway, "upstream_error", "Invalid upstream request")
		return
	}
	query := upstream.URL.Query()
	for _, key := range []string{"after", "limit", "order", "purpose"} {
		for _, value := range request.URL.Query()[key] {
			if key == "after" && strings.HasPrefix(value, "minato_") {
				id, ok := decodeResourceID(value, family)
				if !ok || id.Deployment != candidate.DeploymentID {
					writeError(writer, http.StatusBadRequest, "invalid_cursor", "Resource cursor belongs to a different deployment")
					return
				}
				value = id.Original
			}
			query.Add(key, value)
		}
	}
	upstream.URL.RawQuery = query.Encode()
	if contentType != "" {
		upstream.Header.Set("Content-Type", contentType)
	}
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
		writeError(writer, http.StatusBadGateway, "upstream_error", "Could not reach resource provider")
		return
	}
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 && strings.HasSuffix(path, "/content") {
		writer.Header().Set("Content-Type", response.Header.Get("Content-Type"))
		writer.WriteHeader(response.StatusCode)
		_, _ = io.Copy(writer, response.Body)
		return
	}
	body, err = io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes+1))
	if err != nil || int64(len(body)) > s.config.MaxBodyBytes {
		writeError(writer, http.StatusBadGateway, "upstream_error", "Could not read upstream resource")
		return
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		body = wrapOpenAIResource(body, family, candidate.DeploymentID)
	}
	writeRawResponse(writer, response.StatusCode, response.Header, body)
}

func wrapOpenAIResource(body []byte, family, deployment string) []byte {
	var response map[string]any
	if json.Unmarshal(body, &response) != nil || response == nil {
		return body
	}
	wrap := func(entry map[string]any) {
		if id, ok := entry["id"].(string); ok && id != "" {
			entry["id"] = encodeResourceID(family, deployment, id)
		}
		if family == "batches" {
			for _, field := range []string{"input_file_id", "output_file_id", "error_file_id"} {
				if id, ok := entry[field].(string); ok && id != "" {
					entry[field] = encodeResourceID("files", deployment, id)
				}
			}
		}
	}
	wrap(response)
	for _, field := range []string{"first_id", "last_id"} {
		if id, ok := response[field].(string); ok && id != "" {
			response[field] = encodeResourceID(family, deployment, id)
		}
	}
	if entries, ok := response["data"].([]any); ok {
		for _, raw := range entries {
			if entry, ok := raw.(map[string]any); ok {
				wrap(entry)
			}
		}
	}
	encoded, _ := json.Marshal(response)
	return encoded
}
