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
	"slices"
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

func decodeLiteLLMResourceID(id, family string) (alias, original string, ok bool) {
	prefix := "file-"
	if family == "batches" {
		prefix = "batch_"
	}
	encoded, ok := strings.CutPrefix(id, prefix)
	if !ok || len(encoded) > 8192 {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(encoded, "="))
	if err != nil {
		return "", "", false
	}
	value, ok := strings.CutPrefix(string(raw), "litellm:")
	if !ok {
		return "", "", false
	}
	original, alias, ok = strings.Cut(value, ";model,")
	alias = strings.TrimSpace(alias)
	return alias, original, ok && validResourceSegment(original) && alias != "" && !strings.Contains(alias, ";")
}

func (s *Server) resourceCandidate(alias, deployment string, scope []string) (Candidate, bool) {
	return s.selectResourceCandidate(alias, deployment, "", scope)
}

func (s *Server) selectResourceCandidate(alias, deployment, provider string, scope []string) (Candidate, bool) {
	return s.selectResourceProvider(alias, deployment, provider, scope, false)
}

func (s *Server) selectResourceProvider(alias, deployment, provider string, scope []string, pgVector bool) (Candidate, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var selected Candidate
	for name, route := range s.configuredRoutes {
		if !modelAllowed(scope, name) || (alias != "" && name != alias) {
			continue
		}
		for _, candidate := range route.Candidates {
			compatible := candidate.Provider == "openai" || candidate.Provider == "azure"
			if pgVector {
				compatible = candidate.Provider == "pg_vector"
			}
			if !candidateAllowed(scope, candidate) || !compatible ||
				(provider != "" && candidate.Provider != provider) || candidate.DeploymentID == "" {
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
	provider := request.URL.Query().Get("custom_llm_provider")
	alias := request.URL.Query().Get("model")
	if alias == "" {
		alias = strings.TrimSpace(request.URL.Query().Get("target_model_names"))
	}
	if alias == "" {
		alias = strings.TrimSpace(request.Header.Get("X-LiteLLM-Model"))
	}
	if family == "batches" && s.bedrockAsyncBatchResource(writer, request, alias, provider, scope) {
		return
	}
	if family == "batches" && s.bedrockControlBatchResource(writer, request, alias, provider, scope) {
		return
	}
	if family == "batches" && s.vertexBatchResource(writer, request, alias, provider, scope) {
		return
	}
	if family == "files" && s.bedrockFileContent(writer, request, alias, provider, scope) {
		return
	}
	if family == "files" && s.vertexFileResource(writer, request, alias, provider, scope) {
		return
	}
	var deployment string
	for _, field := range []string{"after", "before"} {
		cursor := request.URL.Query().Get(field)
		if ref, ok := decodeResourceID(cursor, family); ok {
			if deployment != "" && deployment != ref.Deployment {
				writeError(writer, http.StatusBadRequest, "invalid_cursor", "Resource cursors select different deployments")
				return
			}
			deployment = ref.Deployment
		} else if cursorAlias, _, ok := decodeLiteLLMResourceID(cursor, family); ok {
			if alias != "" && alias != cursorAlias {
				writeError(writer, http.StatusBadRequest, "invalid_cursor", "Resource cursor selects a different model")
				return
			}
			alias = cursorAlias
		}
	}
	var formParts []formPart
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
				formParts = form.form
				if requested, _ := form.fields["custom_llm_provider"].(string); requested != "" {
					provider = requested
				}
				if provider == "bedrock" {
					s.uploadBedrockFile(writer, request, alias, scope, form)
					return
				}
				if provider == "vertex_ai" {
					s.uploadVertexFile(writer, request, alias, scope, form)
					return
				}
				if provider != "gemini" {
					body, contentType, err = form.encodeForm("", false, true)
				}
			} else {
				var payload requestPayload
				payload, err = decodeJSONPayload(incoming)
				if err == nil {
					if alias == "" {
						alias, _ = payload.fields["model"].(string)
					}
					bodyProvider, _ := payload.fields["custom_llm_provider"].(string)
					fileID, _ := payload.fields["input_file_id"].(string)
					if provider == "vertex_ai" || bodyProvider == "vertex_ai" ||
						(provider == "" && bodyProvider == "" && strings.HasPrefix(fileID, "gs://")) {
						s.createVertexBatch(writer, request, alias, scope, fileID)
						return
					}
					bedrock := provider == "bedrock" || bodyProvider == "bedrock"
					if provider == "" && bodyProvider == "" && alias != "" && strings.HasPrefix(fileID, "s3://") {
						_, status := s.bedrockControlDeployment(request, alias, "", "", scope)
						bedrock = status == 0
					}
					if bedrock {
						s.createBedrockControlBatch(writer, request, alias, scope, payload.fields)
						return
					}
					if isLegacyManagedFile(fileID) {
						if candidate, original, ok := s.resolveLegacyFile(request, fileID, alias, scope); ok && (deployment == "" || deployment == candidate.DeploymentID) {
							deployment = candidate.DeploymentID
							payload.fields["input_file_id"] = original
						} else {
							err = errors.New("unavailable managed file ID")
						}
					} else if file, ok := decodeResourceID(fileID, "files"); ok {
						deployment = file.Deployment
						payload.fields["input_file_id"] = file.Original
					} else if fileAlias, original, ok := decodeLiteLLMResourceID(fileID, "files"); ok {
						alias = fileAlias
						payload.fields["input_file_id"] = original
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
		if family == "batches" {
			if model, native, managed := decodeLegacyBatchID(decoded); managed {
				if resolved, found := s.legacyBatchDeployment(alias, model, scope); !found || (deployment != "" && deployment != resolved) {
					writeError(writer, http.StatusNotFound, "resource_not_found", "Unknown managed batch deployment")
					return
				} else {
					deployment, decoded = resolved, native
				}
			}
		}
		if family == "files" && isLegacyManagedFile(decoded) {
			candidate, original, found := s.resolveLegacyFile(request, decoded, alias, scope)
			if !found || method != http.MethodGet || !resourceActionAllowed(family, action, method) {
				writeError(writer, http.StatusNotFound, "resource_not_found", "Unknown managed file or resource operation")
				return
			}
			targetPath += "/" + url.PathEscape(original)
			if action != "" { targetPath += "/" + action }
			s.forwardOpenAIResource(writer, request, candidate, family, targetPath, "", nil)
			return
		}
		if resource, ok := decodeResourceID(decoded, family); ok {
			deployment = resource.Deployment
			decoded = resource.Original
		} else if embeddedAlias, original, ok := decodeLiteLLMResourceID(decoded, family); ok {
			alias, decoded = embeddedAlias, original
		} else if strings.HasPrefix(decoded, "minato_") {
			decoded = ""
		}
		if family == "files" && provider == "gemini" {
			decoded = geminiFileID(decoded)
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
	if family == "files" && provider == "anthropic" {
		if candidate, ok := s.anthropicBatchDeployment(alias, deployment, scope); ok {
			s.forwardOpenAIResource(writer, request, candidate, family, targetPath, contentType, body)
			return
		}
		writeError(writer, http.StatusBadRequest, "model_required", "Select a configured Anthropic file deployment")
		return
	}
	if family == "files" && provider == "gemini" {
		if candidate, ok := s.geminiResourceCandidate(alias, deployment, scope); ok {
			s.geminiFile(writer, request, candidate, targetPath, formParts)
			return
		}
		writeError(writer, http.StatusBadRequest, "model_required", "Select a configured Gemini file deployment")
		return
	}
	candidate, ok := s.resourceCandidate(alias, deployment, scope)
	if !ok && family == "files" && provider == "" && (alias != "" || deployment != "") {
		if anthropic, found := s.anthropicBatchDeployment(alias, deployment, scope); found {
			s.forwardOpenAIResource(writer, request, anthropic, family, targetPath, contentType, body)
			return
		}
		if gemini, found := s.geminiResourceCandidate(alias, deployment, scope); found {
			s.geminiFile(writer, request, gemini, targetPath, formParts)
			return
		}
	}
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
	s.forwardResource(writer, request, candidate, family, path, contentType, body, nil)
}

func (s *Server) forwardResource(writer http.ResponseWriter, request *http.Request, candidate Candidate,
	family, path, contentType string, body []byte, transform func([]byte) ([]byte, error)) {
	base := strings.TrimRight(candidate.APIBase, "/")
	target := strings.TrimSuffix(base, "/v1") + path
	if candidate.Provider == "azure" && !strings.HasSuffix(base, "/openai/v1") {
		target = strings.TrimSuffix(base, "/openai") + "/openai" + strings.TrimPrefix(path, "/v1") + "?api-version=" + url.QueryEscape(candidate.APIVersion)
		if (isContainerPath(path) || isFineTuningPath(path)) && slices.Contains([]string{"", "v1", "latest", "preview"}, candidate.APIVersion) {
			target = strings.TrimSuffix(base, "/openai") + "/openai" + path
		}
	}
	upstream, err := http.NewRequestWithContext(request.Context(), request.Method, target, bytes.NewReader(body))
	if err != nil {
		writeError(writer, http.StatusBadGateway, "upstream_error", "Invalid upstream request")
		return
	}
	query := upstream.URL.Query()
	for _, key := range []string{"after", "before", "limit", "order", "order_by", "purpose", "filter", "variant"} {
		for _, value := range request.URL.Query()[key] {
			if key == "after" || key == "before" {
				if family == "containers" {
					deployment := candidate.DeploymentID
					var err error
					value, err = decodeContainerReference(value, &deployment)
					if err != nil {
						writeError(writer, http.StatusBadRequest, "invalid_cursor", "Invalid container cursor")
						return
					}
				} else if strings.HasPrefix(value, "minato_") {
					id, ok := decodeResourceID(value, resourceResultKind(family))
					if !ok || id.Deployment != candidate.DeploymentID {
						writeError(writer, http.StatusBadRequest, "invalid_cursor", "Resource cursor belongs to a different deployment")
						return
					}
					value = id.Original
				} else if family == "files" || family == "batches" {
					if alias, original, ok := decodeLiteLLMResourceID(value, family); ok {
						if alias != candidate.RoutingGroup {
							writeError(writer, http.StatusBadRequest, "invalid_cursor", "Resource cursor belongs to a different model")
							return
						}
						value = original
					}
				}
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
	if candidate.Provider == "anthropic" && family == "files" {
		upstream.Header.Set("x-api-key", key)
		upstream.Header.Set("anthropic-version", "2023-06-01")
		upstream.Header.Set("anthropic-beta", "files-api-2025-04-14")
		for _, name := range []string{"anthropic-version", "anthropic-beta"} {
			if value := request.Header.Get(name); value != "" { upstream.Header.Set(name, value) }
		}
	} else {
		if err := providerKeyHeaders(request.Context(), upstream.Header, candidate, key); err != nil {
			writeError(writer, http.StatusBadGateway, "upstream_error", err.Error())
			return
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
	if response.StatusCode >= 200 && response.StatusCode < 300 && strings.HasSuffix(path, "/content") &&
		((family == "files" && strings.HasPrefix(path, "/v1/files/")) || family == "videos" || family == "container_files") {
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
		if transform != nil {
			body, err = transform(body)
			if err != nil {
				writeError(writer, http.StatusBadGateway, "resource_error", "Could not update resource ownership")
				return
			}
		} else if family != "" {
			if candidate.Provider == "anthropic" && family == "files" {
				body = normalizeAnthropicFiles(body, request.Method)
			}
			body = wrapOpenAIResource(body, family, candidate.DeploymentID)
		}
	}
	writeRawResponse(writer, response.StatusCode, response.Header, body)
}

func wrapOpenAIResource(body []byte, family, deployment string) []byte {
	var response map[string]any
	if json.Unmarshal(body, &response) != nil || response == nil {
		return body
	}
	wrap := func(entry map[string]any) {
		if id, ok := entry["id"].(string); ok && id != "" && family != "vector_stores_search" {
			entry["id"] = encodeResourceID(resourceResultKind(family), deployment, id)
		}
		if family == "batches" {
			for _, field := range []string{"input_file_id", "output_file_id", "error_file_id"} {
				if id, ok := entry[field].(string); ok && id != "" {
					entry[field] = encodeResourceID("files", deployment, id)
				}
			}
		}
		if family == fineTuningFamily {
			for _, field := range []string{"training_file", "validation_file"} {
				if id, ok := entry[field].(string); ok && id != "" {
					entry[field] = encodeResourceID("files", deployment, id)
				}
			}
			if files, ok := entry["result_files"].([]any); ok {
				for index, value := range files {
					if id, ok := value.(string); ok && id != "" {
						files[index] = encodeResourceID("files", deployment, id)
					}
				}
			}
		}
		if family == "container_files" {
			if id, ok := entry["container_id"].(string); ok && id != "" {
				entry["container_id"] = encodeResourceID("containers", deployment, id)
			}
		}
		if family == "eval_runs" {
			if id, ok := entry["eval_id"].(string); ok && id != "" {
				entry["eval_id"] = encodeResourceID("evals", deployment, id)
			}
		}
		if strings.HasPrefix(family, "vector_stores") {
			for field, kind := range map[string]string{"vector_store_id": "vector_stores", "file_id": "files"} {
				if id, ok := entry[field].(string); ok && id != "" {
					entry[field] = encodeResourceID(kind, deployment, id)
				}
			}
		}
	}
	wrap(response)
	if family == anthropicSkillFamily {
		if page, _ := response["next_page"].(string); validResourceSegment(page) {
			response["next_page"] = encodeResourceID(family+"_page", deployment, page)
		}
	}
	for _, field := range []string{"first_id", "last_id"} {
		if id, ok := response[field].(string); ok && id != "" {
			response[field] = encodeResourceID(resourceResultKind(family), deployment, id)
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

func resourceResultKind(family string) string {
	if family == "vector_stores_files" {
		return "files"
	}
	return family
}
