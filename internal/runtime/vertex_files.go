package runtime

import (
	"bytes"
	"encoding/base32"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const vertexFilesPrefix = "litellm-vertex-files/"

var vertexLabelChars = regexp.MustCompile(`[^a-z0-9_-]`)

func vertexStorageObject(candidate Candidate, id string) (bucket, key string, ok bool) {
	uri, err := url.Parse(id)
	if err != nil || uri.Scheme != "gs" || uri.User != nil || uri.RawQuery != "" || uri.Fragment != "" {
		return "", "", false
	}
	bucket, prefix, _ := strings.Cut(candidate.Vertex.Bucket, "/")
	key = strings.TrimPrefix(uri.Path, "/")
	prefix = strings.Trim(prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	if bucket == "" || uri.Host != bucket || !strings.HasPrefix(key, prefix+vertexFilesPrefix) || strings.Contains(key, "\\") {
		return "", "", false
	}
	for _, part := range strings.Split(key, "/") {
		if !validResourceSegment(part) {
			return "", "", false
		}
	}
	return bucket, key, true
}

func vertexStorageBase(candidate Candidate) string {
	if candidate.Vertex.StorageBase != "" {
		return strings.TrimRight(candidate.Vertex.StorageBase, "/")
	}
	return "https://storage.googleapis.com"
}

func (s *Server) uploadVertexFile(w http.ResponseWriter, r *http.Request, alias string, scope []string, form requestPayload) {
	candidate, ok := s.selectGoogleResourceCandidate(alias, "", "vertex_ai", scope, false)
	if !ok || candidate.Vertex == nil || candidate.Vertex.Bucket == "" {
		writeError(w, http.StatusBadRequest, "model_required", "Select a configured Vertex AI file deployment with bucket_name")
		return
	}
	var file []byte
	var filename, purpose string
	for _, part := range form.form {
		switch part.name {
		case "purpose":
			purpose = string(part.body)
		case "file":
			_, params, _ := mime.ParseMediaType(part.header.Get("Content-Disposition"))
			if filename != "" || params["filename"] == "" {
				writeError(w, http.StatusBadRequest, "invalid_request", "Supply exactly one named upload")
				return
			}
			filename, file = params["filename"], part.body
		}
	}
	if filename == "" || len(file) == 0 || purpose == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "A named file and purpose are required")
		return
	}
	object := vertexFilesPrefix + newUUID()
	media := "application/octet-stream"
	if purpose == "batch" {
		model := strings.TrimPrefix(candidate.ModelID, "models/")
		if !validResourceSegment(model) {
			writeError(w, http.StatusBadRequest, "invalid_model", "Vertex batch uploads require a publisher model")
			return
		}
		var err error
		file, err = vertexBatchJSONL(file)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_batch_file", err.Error())
			return
		}
		if int64(len(file)) > s.config.MaxBodyBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Converted Vertex batch file exceeds the body limit")
			return
		}
		object = vertexFilesPrefix + "publishers/google/models/" + model + "/" + newUUID()
		media = "application/jsonl"
	}
	bucket, prefix, _ := strings.Cut(candidate.Vertex.Bucket, "/")
	if prefix = strings.Trim(prefix, "/"); prefix != "" {
		object = prefix + "/" + object
	}
	id := (&url.URL{Scheme: "gs", Host: bucket, Path: "/" + object}).String()
	if _, _, ok := vertexStorageObject(candidate, id); !ok {
		writeError(w, http.StatusBadRequest, "invalid_bucket", "Invalid Vertex file bucket configuration")
		return
	}
	query := url.Values{"name": {object}, "uploadType": {"media"}}
	target := vertexStorageBase(candidate) + "/upload/storage/v1/b/" + url.PathEscape(bucket) + "/o?" + query.Encode()
	raw, ok := s.vertexStorageRequest(w, r, candidate, http.MethodPost, target, media, file)
	if !ok {
		return
	}
	if !s.authorized(r) {
		owner := s.containerOwnerScopes(r, false)[0]
		if err := s.config.ContainerOwners.Record(r.Context(), candidate.DeploymentID, id, owner); err != nil {
			writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not record Vertex file ownership")
			return
		}
	}
	result := vertexFileMetadata(raw, id, purpose)
	result["filename"] = filename
	result["bytes"] = len(file)
	writeJSON(w, http.StatusOK, result)
}

func vertexBatchJSONL(file []byte) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	for line := range bytes.SplitSeq(file, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var item struct {
			ID      string         `json:"custom_id"`
			Body    map[string]any `json:"body"`
			Request map[string]any `json:"request"`
		}
		if json.Unmarshal(line, &item) != nil {
			return nil, errors.New("Invalid Vertex batch JSONL record")
		}
		request := item.Request
		if item.Body != nil {
			request = chatToGemini(item.Body)
		}
		contents, _ := request["contents"].([]any)
		if len(contents) == 0 {
			return nil, errors.New("Vertex batch JSONL requires nonempty messages or native contents")
		}
		if item.ID != "" {
			labels, _ := request["labels"].(map[string]any)
			if labels == nil {
				labels = make(map[string]any)
			}
			label := vertexLabelChars.ReplaceAllString(strings.ToLower(item.ID), "_")
			labels["litellm_custom_id"] = label[:min(len(label), 63)]
			encoded := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(item.ID)))
			for index := 0; len(encoded) > 0; index++ {
				size := min(len(encoded), 59)
				key := "litellm_custom_id_raw"
				if index > 0 {
					key += "_" + strconv.Itoa(index)
				}
				labels[key] = "b32_" + encoded[:size]
				encoded = encoded[size:]
			}
			request["labels"] = labels
		}
		if err := encoder.Encode(map[string]any{"request": request}); err != nil {
			return nil, err
		}
	}
	if output.Len() == 0 {
		return nil, errors.New("Vertex batch JSONL requires at least one record")
	}
	return output.Bytes(), nil
}

func (s *Server) vertexFileResource(w http.ResponseWriter, r *http.Request, alias, provider string, scope []string) bool {
	escaped, ok := strings.CutPrefix(r.URL.EscapedPath(), "/v1/files/")
	if !ok || (provider != "" && provider != "vertex_ai") {
		return false
	}
	contentID, content := strings.CutSuffix(escaped, "/content")
	id, err := url.PathUnescape(contentID)
	if err != nil || !strings.HasPrefix(id, "gs://") {
		return false
	}
	candidate, ok := s.selectGoogleResourceCandidate(alias, "", "vertex_ai", scope, false)
	if !ok || candidate.Vertex == nil {
		writeError(w, http.StatusBadRequest, "model_required", "Select a configured Vertex AI file deployment")
		return true
	}
	bucket, key, ok := vertexStorageObject(candidate, id)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_file_id", "GCS object is outside the managed bucket or prefix")
		return true
	}
	if !s.authorized(r) {
		owners, err := s.config.ContainerOwners.Read(r.Context(), candidate.DeploymentID, []string{id})
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not check Vertex file ownership")
			return true
		}
		if !slices.Contains(s.containerOwnerScopes(r, false), owners[id]) {
			writeError(w, http.StatusNotFound, "resource_not_found", "Unknown Vertex file")
			return true
		}
	}
	target := vertexStorageBase(candidate) + "/storage/v1/b/" + url.PathEscape(bucket) + "/o/" + url.PathEscape(key)
	if content {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Vertex file contents require GET")
			return true
		}
		target += "?alt=media"
	} else if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Unsupported Vertex file operation")
		return true
	}
	raw, ok := s.vertexStorageRequest(w, r, candidate, r.Method, target, "", nil)
	if !ok {
		return true
	}
	switch {
	case r.Method == http.MethodDelete:
		if err := s.config.ContainerOwners.Delete(r.Context(), candidate.DeploymentID, id); err != nil {
			writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not remove Vertex file ownership")
			return true
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "object": "file", "deleted": true})
	case content:
		if strings.HasSuffix(key, "/predictions.jsonl") {
			raw = vertexBatchOutput(raw)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(raw)
	default:
		writeJSON(w, http.StatusOK, vertexFileMetadata(raw, id, "batch"))
	}
	return true
}

func vertexFileMetadata(raw []byte, id, purpose string) map[string]any {
	var details struct {
		Name    string     `json:"name"`
		Size    string     `json:"size"`
		Created *time.Time `json:"timeCreated"`
	}
	_ = json.Unmarshal(raw, &details)
	size, _ := strconv.ParseInt(details.Size, 10, 64)
	created := time.Now().Unix()
	if details.Created != nil {
		created = details.Created.Unix()
	}
	return map[string]any{"id": id, "object": "file", "status": "processed", "purpose": purpose,
		"filename": path.Base(details.Name), "bytes": size, "created_at": created}
}

func vertexBatchOutput(raw []byte) []byte {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	for line := range bytes.SplitSeq(raw, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var item struct {
			Request struct {
				Labels map[string]string `json:"labels"`
			} `json:"request"`
			Status   string         `json:"status"`
			Response map[string]any `json:"response"`
		}
		if json.Unmarshal(line, &item) != nil || (item.Status == "" && item.Response == nil) {
			return raw
		}
		customID := vertexBatchCustomID(item.Request.Labels)
		record := map[string]any{"id": "batch_req_" + newUUID(), "custom_id": customID, "response": nil, "error": nil}
		if item.Status != "" {
			record["error"] = map[string]string{"code": "vertex_ai_error", "message": item.Status}
		} else {
			model, _ := item.Response["modelVersion"].(string)
			model, _, _ = strings.Cut(model, "@")
			response := geminiChatResponse(item.Response, model)
			record["response"] = map[string]any{"status_code": 200, "request_id": response["id"], "body": response}
		}
		if err := encoder.Encode(record); err != nil {
			return raw
		}
	}
	if output.Len() == 0 {
		return raw
	}
	return output.Bytes()
}

func vertexBatchCustomID(labels map[string]string) string {
	raw := labels["litellm_custom_id_raw"]
	if raw == "" {
		if id := labels["litellm_custom_id"]; id != "" {
			return id
		}
		return "unknown"
	}
	var encoded strings.Builder
	chunk := raw
	for i := 1; chunk != ""; i++ {
		data, ok := strings.CutPrefix(chunk, "b32_")
		if !ok {
			return raw
		}
		encoded.WriteString(data)
		chunk = labels["litellm_custom_id_raw_"+strconv.Itoa(i)]
	}
	if decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(encoded.String())); err == nil {
		return string(decoded)
	}
	return raw
}

func (s *Server) vertexStorageRequest(w http.ResponseWriter, r *http.Request, candidate Candidate, method, target, media string, file []byte) ([]byte, bool) {
	request, err := newGoogleResourceRequest(r, candidate, target, file)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not prepare GCS request")
		return nil, false
	}
	request.Method = method
	if media != "" {
		request.Header.Set("Content-Type", media)
	}
	select {
	case s.sem <- struct{}{}:
	case <-r.Context().Done():
		return nil, false
	}
	defer func() { <-s.sem }()
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not reach GCS")
		return nil, false
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes+1))
	if err != nil || int64(len(raw)) > s.config.MaxBodyBytes || response.StatusCode/100 == 3 {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid GCS response")
		return nil, false
	}
	if response.StatusCode/100 != 2 {
		writeRawResponse(w, response.StatusCode, response.Header, raw)
		return nil, false
	}
	return raw, true
}
