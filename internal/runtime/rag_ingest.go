package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strconv"
	"strings"
)

type ragIngestInput struct {
	Options         map[string]any `json:"ingest_options"`
	FileURL         string         `json:"file_url"`
	FileID          string         `json:"file_id"`
	filename, media string
	file            []byte
}

func parseRAGIngest(r *http.Request, limit int64) (ragIngestInput, error) {
	var input ragIngestInput
	body, err := readBody(r, limit)
	if err != nil {
		return input, err
	}
	kind, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if kind != "multipart/form-data" {
		err := json.Unmarshal(body, &input)
		return input, err
	}
	form, err := readMultipart(body, params["boundary"])
	if err != nil {
		return input, err
	}
	for _, part := range form.form {
		switch part.name {
		case "ingest_options":
			if json.Unmarshal(part.body, &input.Options) != nil {
				return input, errors.New("invalid ingest_options")
			}
		case "file_id":
			input.FileID = string(part.body)
		case "file_url":
			input.FileURL = string(part.body)
		case "file":
			_, disposition, _ := mime.ParseMediaType(part.header.Get("Content-Disposition"))
			if input.filename != "" || disposition["filename"] == "" || len(part.body) == 0 {
				return input, errors.New("expected one named RAG file")
			}
			input.filename, input.media, input.file = disposition["filename"], part.header.Get("Content-Type"), part.body
		}
	}
	return input, nil
}

func (input *ragIngestInput) download(r *http.Request, limit int64) error {
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, input.FileURL, nil)
	if err != nil || !validOCRDocumentRequest(request) {
		return errors.New("invalid RAG file URL")
	}
	response, err := ocrDownloadClient.Do(request)
	if err != nil {
		return errors.New("could not download RAG file")
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 || response.ContentLength > limit {
		return errors.New("RAG file could not be downloaded within the body limit")
	}
	input.file, err = io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(input.file)) > limit || len(input.file) == 0 {
		return errors.New("invalid or oversized RAG file")
	}
	input.filename = path.Base(request.URL.Path)
	if input.filename == "." || input.filename == "/" {
		input.filename = "document"
	}
	input.media = response.Header.Get("Content-Type")
	return nil
}

func (s *Server) ragIngest(w http.ResponseWriter, r *http.Request, scope []string) {
	input, err := parseRAGIngest(r, s.config.MaxBodyBytes)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid RAG ingest request")
		return
	}
	store, _ := input.Options["vector_store"].(map[string]any)
	provider, _ := store["custom_llm_provider"].(string)
	if provider == "" {
		provider = "openai"
	}
	if provider != "openai" && provider != "azure" && provider != "gemini" && provider != "bedrock" && provider != "vertex_ai" {
		writeError(w, http.StatusBadRequest, "unsupported_provider", "No RAG ingestion adapter for this provider")
		return
	}
	vectors, _ := store["vector_store_id"].(string)
	alias, _ := store["model"].(string)
	if alias == "" {
		alias = r.URL.Query().Get("model")
	}
	if !modelAllowed(scope, alias) && alias != "" {
		writeError(w, http.StatusForbidden, "model_not_allowed", "The key does not allow this model")
		return
	}
	name, _ := input.Options["name"].(string)
	if name == "" {
		name = "litellm-rag-ingest"
	}
	if input.FileURL != "" {
		if len(input.file) != 0 || input.FileID != "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "Submit one file source")
			return
		}
		if err := input.download(r, s.config.MaxBodyBytes); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
	}
	if len(input.file) != 0 && input.FileID != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "Submit one file source")
		return
	}
	if input.media == "" && len(input.file) != 0 {
		input.media = http.DetectContentType(input.file)
	}
	if provider == "gemini" && (len(input.file) == 0 || input.FileID != "") {
		writeError(w, http.StatusBadRequest, "invalid_request", "Gemini ingestion requires a document upload or URL")
		return
	}
	if provider == "vertex_ai" {
		if _, ok := input.Options["name"].(string); !ok || name == "litellm-rag-ingest" {
			name = "litellm-rag-corpus"
		}
		s.ingestVertexRAG(w, r, scope, alias, vectors, name, store, input)
		return
	}
	if provider == "bedrock" {
		s.ingestBedrockDocument(w, r, scope, alias, vectors, store, input)
		return
	}
	if vectors == "" {
		fields := map[string]any{"name": name, "custom_llm_provider": provider}
		if alias != "" {
			fields["model"] = alias
		}
		body, _ := json.Marshal(fields)
		stage, ok := s.ragStage(w, func(output http.ResponseWriter) {
			s.vectorStoreResource(output, ragSubrequest(r, http.MethodPost, vectorStorePath, body, false), scope)
		})
		if !ok {
			return
		}
		var result struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(stage.body.Bytes(), &result) != nil || result.ID == "" {
			writeError(w, http.StatusBadGateway, "rag_stage_failed", "Vector store creation returned no ID")
			return
		}
		vectors = result.ID
	}
	if provider == "gemini" {
		s.ingestGeminiDocument(w, r, scope, alias, vectors, input)
		return
	}
	s.ingestOpenAIDocument(w, r, scope, alias, provider, vectors, input)
}

func (s *Server) ingestOpenAIDocument(w http.ResponseWriter, r *http.Request, scope []string, alias, provider, vectors string, input ragIngestInput) {
	deployment := ""
	id, err := nativeResourceID(vectors, "vector_stores", &deployment)
	if err != nil || isLegacyVectorStore(id) {
		writeError(w, http.StatusBadRequest, "invalid_request", "RAG ingestion requires a writable vector store ID")
		return
	}
	candidate, ok := s.selectResourceCandidate(alias, deployment, provider, scope)
	if !ok {
		writeError(w, http.StatusBadRequest, "model_required", "Select one configured ingestion deployment")
		return
	}
	fileID := input.FileID
	if len(input.file) != 0 {
		var multipartBody bytes.Buffer
		form := multipart.NewWriter(&multipartBody)
		disposition := mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": input.filename})
		part, formErr := form.CreatePart(textproto.MIMEHeader{"Content-Disposition": {disposition}, "Content-Type": {input.media}})
		if formErr == nil {
			_, formErr = part.Write(input.file)
		}
		if formErr == nil {
			formErr = form.WriteField("purpose", "assistants")
		}
		if formErr == nil {
			formErr = form.Close()
		}
		if formErr != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "Could not encode the RAG file upload")
			return
		}
		stage, ok := s.ragStage(w, func(output http.ResponseWriter) {
			s.forwardOpenAIResource(output, ragSubrequest(r, http.MethodPost, "/v1/files", nil, false), candidate, "files", "/v1/files", form.FormDataContentType(), multipartBody.Bytes())
		})
		if !ok {
			return
		}
		var result struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(stage.body.Bytes(), &result) != nil || result.ID == "" {
			writeError(w, http.StatusBadGateway, "rag_stage_failed", "File upload returned no ID")
			return
		}
		fileID = result.ID
	}
	if fileID != "" {
		owner := candidate.DeploymentID
		native, err := nativeResourceID(fileID, "files", &owner)
		if err != nil || isLegacyManagedFile(native) {
			writeError(w, http.StatusBadRequest, "invalid_request", "File belongs to a different vector store deployment")
			return
		}
		payload, _ := json.Marshal(map[string]string{"file_id": native})
		target := vectorStorePath + "/" + url.PathEscape(id) + "/files"
		if _, ok := s.ragStage(w, func(output http.ResponseWriter) {
			s.forwardOpenAIResource(output, ragSubrequest(r, http.MethodPost, target, nil, false), candidate, "vector_stores_files", target, "application/json", payload)
		}); !ok {
			return
		}
	}
	writeRAGIngest(w, vectors, fileID)
}

func (s *Server) ingestGeminiDocument(w http.ResponseWriter, r *http.Request, scope []string, alias, vectors string, input ragIngestInput) {
	deployment := ""
	id, err := nativeResourceID(strings.TrimPrefix(vectors, geminiStorePrefix), "vector_stores", &deployment)
	if err != nil || isLegacyVectorStore(id) {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid Gemini vector store ID")
		return
	}
	candidate, ok := s.geminiResourceCandidate(alias, deployment, scope)
	if !ok {
		writeError(w, http.StatusBadRequest, "model_required", "Select a configured Gemini ingestion deployment")
		return
	}
	base := strings.TrimSuffix(strings.TrimRight(candidate.APIBase, "/"), "/v1beta")
	metadata := map[string]any{"displayName": input.filename}
	if configuration, _ := input.Options["vector_store"].(map[string]any); configuration != nil {
		if custom := configuration["custom_metadata"]; custom != nil {
			metadata["customMetadata"] = custom
		}
	}
	if strategy, _ := input.Options["chunking_strategy"].(map[string]any); strategy != nil {
		if whitespace, _ := strategy["white_space_config"].(map[string]any); whitespace != nil {
			cfg := map[string]any{"maxTokensPerChunk": 800, "maxOverlapTokens": 400}
			for from, to := range map[string]string{"max_tokens_per_chunk": "maxTokensPerChunk", "max_overlap_tokens": "maxOverlapTokens"} {
				if value := whitespace[from]; value != nil {
					cfg[to] = value
				}
			}
			metadata["chunkingConfig"] = map[string]any{"whiteSpaceConfig": cfg}
		}
	}
	body, _ := json.Marshal(metadata)
	start, err := newGoogleResourceRequest(r, candidate, base+"/upload/v1beta/"+geminiStorePrefix+url.PathEscape(id)+":uploadToFileSearchStore", body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not initiate Gemini file search upload")
		return
	}
	start.Header.Set("Content-Type", "application/json")
	start.Header.Set("X-Goog-Upload-Protocol", "resumable")
	start.Header.Set("X-Goog-Upload-Command", "start")
	start.Header.Set("X-Goog-Upload-Header-Content-Length", strconv.Itoa(len(input.file)))
	start.Header.Set("X-Goog-Upload-Header-Content-Type", input.media)
	select {
	case s.sem <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	defer func() { <-s.sem }()
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	status, data, headers, err := s.sendGeminiFileRequest(&client, start)
	if !writeGeminiFileError(w, status, data, headers, err) {
		return
	}
	target, err := url.Parse(headers.Get("X-Goog-Upload-Url"))
	if err != nil || target.User != nil || target.Fragment != "" || target.Scheme != start.URL.Scheme || !strings.EqualFold(target.Host, start.URL.Host) {
		writeError(w, http.StatusBadGateway, "upstream_error", "Gemini upload session must use the configured origin")
		return
	}
	finish, err := http.NewRequestWithContext(r.Context(), http.MethodPut, target.String(), bytes.NewReader(input.file))
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Gemini upload session")
		return
	}
	finish.Header.Set("X-Goog-Upload-Offset", "0")
	finish.Header.Set("X-Goog-Upload-Command", "upload, finalize")
	status, data, headers, err = s.sendGeminiFileRequest(&client, finish)
	if !writeGeminiFileError(w, status, data, headers, err) {
		return
	}
	var operation struct {
		Name string `json:"name"`
		File struct {
			Name string `json:"name"`
		} `json:"file"`
	}
	if json.Unmarshal(data, &operation) != nil || (operation.Name == "" && operation.File.Name == "") {
		writeError(w, http.StatusBadGateway, "upstream_error", "Gemini ingestion returned no operation or file ID")
		return
	}
	file := operation.Name
	if file == "" {
		file = operation.File.Name
	}
	writeRAGIngest(w, vectors, file)
}

func writeRAGIngest(w http.ResponseWriter, vectors, file string) {
	var id any
	if file != "" {
		id = file
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": "ingest_" + newUUID(), "status": "completed", "vector_store_id": vectors, "file_id": id})
}
