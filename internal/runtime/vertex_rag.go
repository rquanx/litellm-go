package runtime

import (
	"bytes"
	"encoding/json"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"regexp"
	"strings"
	"time"
)

var vertexRAGComponent = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func vertexRAGPrefix(candidate Candidate) string {
	return "projects/" + candidate.Vertex.Project + "/locations/" + candidate.Vertex.Location
}

func vertexRAGCorpus(candidate Candidate, id string) (string, bool) {
	prefix := vertexRAGPrefix(candidate) + "/ragCorpora/"
	suffix := strings.TrimPrefix(id, prefix)
	return prefix + suffix, vertexRAGComponent.MatchString(suffix)
}

func (s *Server) vertexRAGRequest(w http.ResponseWriter, r *http.Request, candidate Candidate, method, target, media string, body []byte) ([]byte, bool) {
	base := strings.TrimRight(candidate.APIBase, "/")
	base = strings.TrimSuffix(strings.TrimSuffix(base, "/v1beta1"), "/v1")
	upstream, err := newGoogleResourceRequest(r, candidate, base+target, body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not prepare Vertex RAG request")
		return nil, false
	}
	upstream.Method = method
	if media != "" {
		upstream.Header.Set("Content-Type", media)
	}
	if strings.HasPrefix(media, "multipart/form-data") {
		upstream.Header.Set("X-Goog-Upload-Protocol", "multipart")
	}
	select {
	case s.sem <- struct{}{}:
	case <-r.Context().Done():
		return nil, false
	}
	defer func() { <-s.sem }()
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	status, raw, headers, err := s.sendGeminiFileRequest(&client, upstream)
	if err != nil || status/100 == 3 {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Vertex RAG response")
		return nil, false
	}
	if status/100 != 2 {
		writeRawResponse(w, status, headers, raw)
		return nil, false
	}
	return raw, true
}

func (s *Server) searchVertexRAG(w http.ResponseWriter, r *http.Request, candidate Candidate, id string, fields map[string]any) {
	corpus, valid := vertexRAGCorpus(candidate, id)
	query, ok := geminiVectorQuery(fields["query"])
	if !valid || !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "A Vertex corpus in the configured project and a query are required")
		return
	}
	retrieval := make(map[string]any)
	for from, to := range map[string]string{"max_num_results": "top_k", "filters": "filter", "ranking_options": "ranking"} {
		if fields[from] != nil {
			retrieval[to] = fields[from]
		}
	}
	queryFields := map[string]any{"text": query}
	if len(retrieval) > 0 {
		queryFields["rag_retrieval_config"] = retrieval
	}
	payload, _ := json.Marshal(map[string]any{
		"vertex_rag_store": map[string]any{"rag_resources": []map[string]string{{"rag_corpus": corpus}}},
		"query":            queryFields,
	})
	raw, ok := s.vertexRAGRequest(w, r, candidate, http.MethodPost, "/v1/"+vertexRAGPrefix(candidate)+":retrieveContexts", "application/json", payload)
	if !ok {
		return
	}
	var result struct {
		Contexts struct {
			Items []struct {
				Text        string         `json:"text"`
				URI         string         `json:"sourceUri"`
				DisplayName string         `json:"sourceDisplayName"`
				Score       float64        `json:"score"`
				PageSpan    map[string]any `json:"pageSpan"`
			} `json:"contexts"`
		} `json:"contexts"`
	}
	if json.Unmarshal(raw, &result) != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Vertex RAG search result")
		return
	}
	data := make([]map[string]any, 0, len(result.Contexts.Items))
	for _, item := range result.Contexts.Items {
		id, name := item.URI, item.DisplayName
		attributes := make(map[string]any)
		if id != "" {
			attributes["sourceUri"] = id
		} else {
			id = name
		}
		if name != "" {
			attributes["sourceDisplayName"] = name
		} else {
			name = "Unknown Document"
		}
		if len(item.PageSpan) > 0 {
			attributes["pageSpan"] = item.PageSpan
		}
		data = append(data, map[string]any{"file_id": id, "filename": name, "score": item.Score, "attributes": attributes,
			"content": []map[string]string{{"type": "text", "text": item.Text}}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "vector_store.search_results.page", "search_query": query, "data": data})
}

type vertexRAGOperation struct {
	Name     string `json:"name"`
	Done     bool   `json:"done"`
	Response struct {
		Name string `json:"name"`
	} `json:"response"`
	Error json.RawMessage `json:"error"`
}

func (s *Server) createVertexRAGCorpus(w http.ResponseWriter, r *http.Request, candidate Candidate, payload map[string]any) (string, bool) {
	body, _ := json.Marshal(payload)
	parent := vertexRAGPrefix(candidate)
	raw, ok := s.vertexRAGRequest(w, r, candidate, http.MethodPost, "/v1beta1/"+parent+"/ragCorpora", "application/json", body)
	if !ok {
		return "", false
	}
	var operation vertexRAGOperation
	for attempt := 0; attempt <= 30; attempt++ {
		operation = vertexRAGOperation{}
		if json.Unmarshal(raw, &operation) != nil {
			break
		}
		if operation.Done {
			if corpus, valid := vertexRAGCorpus(candidate, operation.Response.Name); valid && operation.Response.Name == corpus && len(operation.Error) == 0 {
				return corpus, true
			}
			break
		}
		operationID, matches := strings.CutPrefix(operation.Name, parent+"/operations/")
		if !matches || !vertexRAGComponent.MatchString(operationID) || attempt == 30 {
			break
		}
		if attempt > 0 {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
				return "", false
			}
		}
		raw, ok = s.vertexRAGRequest(w, r, candidate, http.MethodGet, "/v1beta1/"+operation.Name, "", nil)
		if !ok {
			return "", false
		}
	}
	writeError(w, http.StatusBadGateway, "upstream_error", "Vertex RAG corpus creation did not complete successfully")
	return "", false
}

func (s *Server) ingestVertexRAG(w http.ResponseWriter, r *http.Request, scope []string, alias, id, name string, options map[string]any, input ragIngestInput) {
	candidate, ok := s.selectGoogleResourceCandidate(alias, "", "vertex_ai", scope, false)
	if !ok || candidate.Vertex == nil {
		writeError(w, http.StatusBadRequest, "model_required", "Select a configured Vertex AI deployment")
		return
	}
	corpus, valid := vertexRAGCorpus(candidate, id)
	if (id != "" && !valid) || (len(input.file) == 0 && input.FileID == "") {
		writeError(w, http.StatusBadRequest, "invalid_request", "Submit a file and a corpus in the configured Vertex project")
		return
	}
	if len(input.file) != 0 && (!validResourceSegment(input.filename) || strings.ContainsAny(input.filename, "\r\n\x00")) {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid Vertex document filename")
		return
	}
	if id == "" {
		payload := map[string]any{"displayName": name}
		if value := options["description"]; value != nil {
			payload["description"] = value
		}
		database, _ := options["vector_db_config"].(map[string]any)
		if value, _ := options["embedding_model"].(string); value != "" {
			if database == nil {
				database = make(map[string]any)
			}
			database["ragEmbeddingModelConfig"] = map[string]any{"vertexPredictionEndpoint": map[string]string{"endpoint": value}}
		}
		if len(database) > 0 {
			payload["vectorDbConfig"] = database
		}
		corpus, ok = s.createVertexRAGCorpus(w, r, candidate, payload)
		if !ok {
			return
		}
		id = corpus
	}
	if len(input.file) == 0 {
		writeRAGIngest(w, id, input.FileID)
		return
	}
	file := map[string]any{"display_name": input.filename}
	if value := options["file_description"]; value != nil {
		file["description"] = value
	}
	metadata := map[string]any{"rag_file": file}
	if strategy, _ := input.Options["chunking_strategy"].(map[string]any); strategy != nil {
		chunks := make(map[string]any)
		for _, field := range []string{"chunk_size", "chunk_overlap"} {
			if value, _ := strategy[field].(float64); value > 0 {
				chunks[field] = value
			}
		}
		if len(chunks) > 0 {
			metadata["upload_rag_file_config"] = map[string]any{"rag_file_transformation_config": map[string]any{
				"rag_file_chunking_config": map[string]any{"fixed_length_chunking": chunks},
			}}
		}
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="metadata"`}, "Content-Type": {"application/json"}})
	if err == nil {
		err = json.NewEncoder(part).Encode(metadata)
	}
	if err == nil {
		disposition := mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": input.filename})
		part, err = form.CreatePart(textproto.MIMEHeader{"Content-Disposition": {disposition}, "Content-Type": {input.media}})
	}
	if err == nil {
		_, err = part.Write(input.file)
	}
	if err == nil {
		err = form.Close()
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "Could not encode the Vertex RAG file")
		return
	}
	raw, ok := s.vertexRAGRequest(w, r, candidate, http.MethodPost, "/upload/v1beta1/"+corpus+"/ragFiles:upload", form.FormDataContentType(), body.Bytes())
	if !ok {
		return
	}
	var result struct {
		Name string `json:"name"`
		File struct {
			Name string `json:"name"`
		} `json:"ragFile"`
	}
	if json.Unmarshal(raw, &result) != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Vertex RAG file result")
		return
	}
	fileID := result.File.Name
	if fileID == "" {
		fileID = result.Name
	}
	if fileID == "" {
		writeError(w, http.StatusBadGateway, "upstream_error", "Vertex RAG returned no file ID")
		return
	}
	writeRAGIngest(w, id, fileID)
}

func vertexRAGStorePath(id string) bool {
	segments := strings.Split(id, "/")
	return len(segments) == 6 && segments[0] == "projects" && segments[2] == "locations" && segments[4] == "ragCorpora" &&
		vertexRAGComponent.MatchString(segments[1]) && vertexRAGComponent.MatchString(segments[3]) && vertexRAGComponent.MatchString(segments[5])
}
