package runtime

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

const vectorStorePath = "/v1/vector_stores"

func isVectorStorePath(path string) bool {
	return path == vectorStorePath || strings.HasPrefix(path, vectorStorePath+"/")
}

func nativeResourceID(id, family string, deployment *string) (string, error) {
	if strings.HasPrefix(id, "minato_") {
		resource, ok := decodeResourceID(id, family)
		if !ok || (*deployment != "" && *deployment != resource.Deployment) {
			return "", errors.New("invalid or mismatched resource ID")
		}
		*deployment = resource.Deployment
		id = resource.Original
	}
	if !validResourceSegment(id) {
		return "", errors.New("invalid resource ID")
	}
	return id, nil
}

func vectorStoreOperation(path, method string, deployment *string) (string, string, error) {
	const stores = "vector_stores"
	if path == vectorStorePath && (method == http.MethodGet || method == http.MethodPost) {
		return path, stores, nil
	}
	parts := strings.Split(strings.TrimPrefix(path, vectorStorePath+"/"), "/")
	id, err := url.PathUnescape(parts[0])
	if err == nil {
		id = strings.TrimPrefix(id, geminiStorePrefix)
		if !(len(parts) == 2 && parts[1] == "search" && method == http.MethodPost && vertexRAGStorePath(id)) {
			id, err = nativeResourceID(id, stores, deployment)
		}
	}
	if err != nil {
		return "", "", err
	}
	target := vectorStorePath + "/" + url.PathEscape(id)
	switch {
	case len(parts) == 1 && (method == http.MethodGet || method == http.MethodPost || method == http.MethodDelete):
		return target, stores, nil
	case len(parts) == 2 && parts[1] == "search" && method == http.MethodPost:
		return target + "/search", "vector_stores_search", nil
	case len(parts) == 2 && parts[1] == "files" && (method == http.MethodGet || method == http.MethodPost):
		return target + "/files", "vector_stores_files", nil
	case len(parts) >= 3 && len(parts) <= 4 && parts[1] == "files":
		fileID, err := url.PathUnescape(parts[2])
		if err == nil {
			fileID, err = nativeResourceID(fileID, "files", deployment)
		}
		if err != nil {
			return "", "", err
		}
		target += "/files/" + url.PathEscape(fileID)
		if len(parts) == 3 && (method == http.MethodGet || method == http.MethodPost || method == http.MethodDelete) {
			return target, "vector_stores_files", nil
		}
		if len(parts) == 4 && parts[3] == "content" && method == http.MethodGet {
			return target + "/content", "vector_stores_files", nil
		}
	}
	return "", "", errors.New("unknown vector store operation")
}

func (s *Server) vectorStoreResource(w http.ResponseWriter, request *http.Request, scope []string) {
	var deployment string
	path, family, err := vectorStoreOperation(request.URL.EscapedPath(), request.Method, &deployment)
	if err != nil {
		writeError(w, http.StatusNotFound, "resource_not_found", "Unknown vector store operation or resource ID")
		return
	}
	query := request.URL.Query()
	cursorFamily := resourceResultKind(family)
	for _, name := range []string{"after", "before"} {
		for _, id := range query[name] {
			if _, err := nativeResourceID(id, cursorFamily, &deployment); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_cursor", "Invalid or mismatched vector store cursor")
				return
			}
		}
	}
	alias := query.Get("model")
	if alias == "" {
		alias = query.Get("target_model_names")
	}
	provider := query.Get("custom_llm_provider")
	var fields map[string]any
	var body []byte
	contentType := ""
	if request.Method == http.MethodPost {
		incoming, err := readBody(request, s.config.MaxBodyBytes)
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large")
			return
		}
		payload, err := decodeJSONPayload(incoming)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Expected a vector store JSON request")
			return
		}
		bodyAlias, err := vectorStoreModel(payload.fields)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if bodyAlias != "" {
			if alias != "" && bodyAlias != alias {
				writeError(w, http.StatusBadRequest, "invalid_request", "Conflicting vector store model selectors")
				return
			}
			alias = bodyAlias
		}
		if err := rewriteVectorStoreFiles(payload.fields, &deployment); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		fields = payload.fields
		if requested, _ := fields["custom_llm_provider"].(string); requested != "" {
			provider = requested
		}
		delete(fields, "custom_llm_provider")
		body, _ = json.Marshal(fields)
		contentType = "application/json"
	}
	escapedID, suffix, _ := strings.Cut(strings.TrimPrefix(path, vectorStorePath+"/"), "/")
	id, _ := url.PathUnescape(escapedID)
	if isLegacyVectorStore(id) {
		if !(request.Method == http.MethodGet && suffix == "") && family != "vector_stores_search" {
			writeError(w, http.StatusBadRequest, "unsupported_endpoint", "Managed vector stores support retrieval and search")
			return
		}
		candidate, original, ok := s.resolveLegacyVectorStore(request, id, alias, deployment, scope)
		if !ok || (provider != "" && provider != candidate.Provider) {
			writeError(w, http.StatusNotFound, "resource_not_found", "Unknown managed vector store")
			return
		}
		path = vectorStorePath + "/" + url.PathEscape(original)
		if suffix != "" {
			path += "/" + suffix
		}
		if candidate.Provider == "gemini" {
			s.geminiVectorStore(w, request, candidate, family, path, fields)
			return
		}
		if family == "vector_stores" {
			s.forwardResource(w, request, candidate, family, path, contentType, body, func(data []byte) ([]byte, error) {
				var result map[string]any
				if err := json.Unmarshal(data, &result); err != nil {
					return nil, err
				}
				if result == nil {
					return nil, errors.New("expected a vector store object")
				}
				result["id"] = id
				return json.Marshal(result)
			})
			return
		}
		s.forwardOpenAIResource(w, request, candidate, family, path, contentType, body)
		return
	}
	if provider == "gemini" {
		if candidate, ok := s.geminiResourceCandidate(alias, deployment, scope); ok {
			s.geminiVectorStore(w, request, candidate, family, path, fields)
			return
		}
		writeError(w, http.StatusBadRequest, "model_required", "Select one configured Gemini vector store deployment")
		return
	}
	if provider == "vertex_ai" {
		candidate, ok := s.selectGoogleResourceCandidate(alias, deployment, provider, scope, false)
		if !ok || candidate.Vertex == nil {
			writeError(w, http.StatusBadRequest, "model_required", "Select a configured Vertex AI deployment")
			return
		}
		switch {
		case family == "vector_stores_search":
			s.searchVertexRAG(w, request, candidate, id, fields)
		case path == vectorStorePath && request.Method == http.MethodPost:
			s.createVertexVectorStore(w, request, candidate, fields)
		default:
			writeError(w, http.StatusBadRequest, "unsupported_endpoint", "Vertex AI supports vector store creation and search")
		}
		return
	}
	if vertexRAGStorePath(id) {
		writeError(w, http.StatusBadRequest, "invalid_request", "A Vertex corpus requires the vertex_ai provider")
		return
	}
	if provider == "s3_vectors" {
		if family != "vector_stores_search" {
			writeError(w, http.StatusBadRequest, "unsupported_endpoint", "S3 Vectors supports vector search")
			return
		}
		s.searchS3Vectors(w, request, scope, alias, id, fields)
		return
	}
	if provider == "bedrock" {
		if family != "vector_stores_search" {
			writeError(w, http.StatusBadRequest, "unsupported_endpoint", "Bedrock knowledge bases support vector search")
			return
		}
		s.searchBedrockKnowledgeBase(w, request, alias, id, scope, fields)
		return
	}
	candidate, ok := s.selectResourceCandidate(alias, deployment, provider, scope)
	if !ok && provider == "" && (alias != "" || deployment != "") {
		if gemini, found := s.geminiResourceCandidate(alias, deployment, scope); found {
			s.geminiVectorStore(w, request, gemini, family, path, fields)
			return
		}
	}
	if !ok {
		writeError(w, http.StatusBadRequest, "model_required", "Select one configured vector store deployment")
		return
	}
	s.forwardOpenAIResource(w, request, candidate, family, path, contentType, body)
}

func vectorStoreModel(fields map[string]any) (string, error) {
	model, _ := fields["model"].(string)
	delete(fields, "model")
	if target, ok := fields["target_model_names"]; ok && target != nil {
		var targetModel string
		switch value := target.(type) {
		case string:
			targetModel = strings.TrimSpace(value)
		case []any:
			if len(value) == 1 {
				targetModel, _ = value[0].(string)
			}
		}
		if targetModel == "" || strings.Contains(targetModel, ",") {
			return "", errors.New("select one target model for the vector store")
		}
		if model != "" && model != targetModel {
			return "", errors.New("conflicting vector store model selectors")
		}
		model = targetModel
	}
	delete(fields, "target_model_names")
	return model, nil
}

func rewriteVectorStoreFiles(fields map[string]any, deployment *string) error {
	if value, present := fields["file_id"]; present && value != nil {
		id, _ := value.(string)
		resolved, err := nativeResourceID(id, "files", deployment)
		if err != nil {
			return err
		}
		fields["file_id"] = resolved
	}
	if value, present := fields["file_ids"]; present && value != nil {
		files, ok := value.([]any)
		if !ok {
			return errors.New("file_ids must be an array")
		}
		for index, value := range files {
			id, _ := value.(string)
			resolved, err := nativeResourceID(id, "files", deployment)
			if err != nil {
				return err
			}
			files[index] = resolved
		}
	}
	return nil
}

func (s *Server) rewriteResponseVectorStores(r *http.Request, fields map[string]any, alias string, scope []string, deployment *string) error {
	tools, _ := fields["tools"].([]any)
	type reference struct {
		stores []any
		index  int
		id     string
	}
	var legacy []reference
	for _, tool := range tools {
		value, _ := tool.(map[string]any)
		if value["type"] != "file_search" {
			continue
		}
		stores, _ := value["vector_store_ids"].([]any)
		for index, store := range stores {
			id, _ := store.(string)
			switch {
			case strings.HasPrefix(id, "minato_"):
				original, err := nativeResourceID(id, "vector_stores", deployment)
				if err != nil {
					return err
				}
				stores[index] = original
			case isLegacyVectorStore(id):
				legacy = append(legacy, reference{stores, index, id})
			}
		}
	}
	for _, ref := range legacy {
		candidate, original, ok := s.resolveLegacyVectorStore(r, ref.id, alias, *deployment, scope)
		if !ok || candidate.Endpoint != "responses" {
			return errors.New("unknown or mismatched managed vector store")
		}
		*deployment = candidate.DeploymentID
		ref.stores[ref.index] = original
	}
	return nil
}
