package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const vertexBatchFamily = "vertex_batch"

type vertexBatch struct {
	Name    string     `json:"name"`
	State   string     `json:"state"`
	Created *time.Time `json:"createTime"`
	Input   struct {
		GCS struct {
			URIs []string `json:"uris"`
		} `json:"gcsSource"`
	} `json:"inputConfig"`
	Output struct {
		GCS struct {
			Prefix string `json:"outputUriPrefix"`
		} `json:"gcsDestination"`
	} `json:"outputConfig"`
	OutputInfo struct {
		Directory string `json:"gcsOutputDirectory"`
	} `json:"outputInfo"`
}

func vertexBatchPath(candidate Candidate) string {
	return "/v1/projects/" + url.PathEscape(candidate.Vertex.Project) + "/locations/" + url.PathEscape(candidate.Vertex.Location) + "/batchPredictionJobs"
}

func (job vertexBatch) response(candidate Candidate) map[string]any {
	_, id, ok := strings.Cut(job.Name, "/batchPredictionJobs/")
	if !ok || !validResourceSegment(id) {
		return nil
	}
	status := map[string]string{"JOB_STATE_QUEUED": "validating", "JOB_STATE_PENDING": "validating", "JOB_STATE_RUNNING": "in_progress",
		"JOB_STATE_SUCCEEDED": "completed", "JOB_STATE_PARTIALLY_SUCCEEDED": "completed", "JOB_STATE_CANCELLING": "cancelling",
		"JOB_STATE_CANCELLED": "cancelled", "JOB_STATE_PAUSED": "in_progress", "JOB_STATE_UPDATING": "in_progress", "JOB_STATE_EXPIRED": "expired"}[job.State]
	if status == "" {
		status = "failed"
	}
	input, output := "", job.OutputInfo.Directory
	if len(job.Input.GCS.URIs) != 0 {
		input = job.Input.GCS.URIs[0]
	}
	if output == "" {
		output = job.Output.GCS.Prefix
	}
	if output != "" && !strings.HasSuffix(output, "/predictions.jsonl") {
		output = strings.TrimRight(output, "/") + "/predictions.jsonl"
	}
	var created int64
	if job.Created != nil {
		created = job.Created.Unix()
	}
	return map[string]any{"id": encodeResourceID(vertexBatchFamily, candidate.DeploymentID, id), "object": "batch", "status": status,
		"completion_window": "24hrs", "endpoint": "", "created_at": created, "input_file_id": input, "output_file_id": output, "error_file_id": nil}
}

func (s *Server) createVertexBatch(w http.ResponseWriter, r *http.Request, alias string, scope []string, fileID string) {
	candidate, ok := s.selectGoogleResourceCandidate(alias, "", "vertex_ai", scope, false)
	if !ok || candidate.Vertex == nil {
		writeError(w, http.StatusBadRequest, "model_required", "Select a configured Vertex AI batch model")
		return
	}
	input, err := url.Parse(fileID)
	if err != nil || input.Scheme != "gs" || input.Hostname() == "" || input.User != nil || input.RawQuery != "" ||
		input.Fragment != "" || input.Path == "" || strings.HasSuffix(input.Path, "/") {
		writeError(w, http.StatusBadRequest, "invalid_request", "Vertex AI batches require a GCS object URI")
		return
	}
	if !s.authorized(r) {
		if _, _, managed := vertexStorageObject(candidate, fileID); managed {
			owners, err := s.config.ContainerOwners.Read(r.Context(), candidate.DeploymentID, []string{fileID})
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not check Vertex file ownership")
				return
			}
			if !slices.Contains(s.containerOwnerScopes(r, false), owners[fileID]) {
				writeError(w, http.StatusNotFound, "resource_not_found", "Unknown Vertex batch input")
				return
			}
		}
	}
	prefix := fileID[:strings.LastIndexByte(fileID, '/')]
	model := strings.TrimPrefix(candidate.ModelID, "models/")
	if !strings.HasPrefix(model, "publishers/") && !strings.HasPrefix(model, "projects/") {
		model = "publishers/google/models/" + model
	}
	payload := map[string]any{
		"displayName": "litellm-vertex-batch-" + newUUID(), "model": model,
		"inputConfig":  map[string]any{"gcsSource": map[string]any{"uris": []string{fileID}}, "instancesFormat": "jsonl"},
		"outputConfig": map[string]any{"gcsDestination": map[string]string{"outputUriPrefix": prefix}, "predictionsFormat": "jsonl"},
	}
	raw, ok := s.sendVertexBatch(w, r, candidate, http.MethodPost, vertexBatchPath(candidate), payload)
	if !ok {
		return
	}
	var job vertexBatch
	if json.Unmarshal(raw, &job) != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Vertex batch response")
		return
	}
	result := job.response(candidate)
	if result == nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Vertex batch identifier")
		return
	}
	if !s.authorized(r) {
		ref, _ := decodeResourceID(result["id"].(string), vertexBatchFamily)
		owner := s.containerOwnerScopes(r, false)[0]
		if err := s.config.ContainerOwners.Record(r.Context(), candidate.DeploymentID, "vertex_batch:"+ref.Original, owner); err != nil {
			writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not record Vertex batch ownership")
			return
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) vertexBatchResource(w http.ResponseWriter, r *http.Request, alias, provider string, scope []string) bool {
	if r.URL.Path == "/v1/batches" {
		if r.Method == http.MethodGet && provider == "vertex_ai" {
			s.listVertexBatches(w, r, alias, scope)
			return true
		}
		return false
	}
	raw, action, _ := strings.Cut(strings.TrimPrefix(r.URL.EscapedPath(), "/v1/batches/"), "/")
	id, err := url.PathUnescape(raw)
	ref, bound := decodeResourceID(id, vertexBatchFamily)
	if err != nil || (!bound && provider != "vertex_ai") || (provider != "" && provider != "vertex_ai") {
		return false
	}
	if bound {
		id = ref.Original
	}
	candidate, ok := s.selectGoogleResourceCandidate(alias, ref.Deployment, "vertex_ai", scope, false)
	if !ok || candidate.Vertex == nil || !validResourceSegment(id) {
		writeError(w, http.StatusNotFound, "resource_not_found", "Unknown Vertex batch")
		return true
	}
	if !s.authorized(r) {
		key := "vertex_batch:" + id
		owners, err := s.config.ContainerOwners.Read(r.Context(), candidate.DeploymentID, []string{key})
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not check Vertex batch ownership")
			return true
		}
		if !slices.Contains(s.containerOwnerScopes(r, false), owners[key]) {
			writeError(w, http.StatusNotFound, "resource_not_found", "Unknown Vertex batch")
			return true
		}
	}
	path := vertexBatchPath(candidate) + "/" + url.PathEscape(id)
	if r.Method == http.MethodPost && action == "cancel" {
		if _, ok := s.sendVertexBatch(w, r, candidate, http.MethodPost, path+":cancel", map[string]any{}); !ok {
			return true
		}
	} else if r.Method != http.MethodGet || action != "" {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Unsupported Vertex batch operation")
		return true
	}
	result, ok := s.sendVertexBatch(w, r, candidate, http.MethodGet, path, nil)
	if ok {
		var job vertexBatch
		if json.Unmarshal(result, &job) != nil {
			writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Vertex batch response")
		} else if response := job.response(candidate); response != nil && response["id"] == encodeResourceID(vertexBatchFamily, candidate.DeploymentID, id) {
			if response["status"] == "completed" && !s.authorized(r) {
				output, _ := response["output_file_id"].(string)
				if _, _, managed := vertexStorageObject(candidate, output); managed {
					owners, lookupErr := s.config.ContainerOwners.Read(r.Context(), candidate.DeploymentID, []string{"vertex_batch:" + id})
					owner := owners["vertex_batch:"+id]
					if lookupErr != nil || owner == "" || s.config.ContainerOwners.Record(r.Context(), candidate.DeploymentID, output, owner) != nil {
						writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not record Vertex output ownership")
						return true
					}
				}
			}
			writeJSON(w, http.StatusOK, response)
		} else {
			writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Vertex batch identifier")
		}
	}
	return true
}

func (s *Server) listVertexBatches(w http.ResponseWriter, r *http.Request, alias string, scope []string) {
	candidate, ok := s.selectGoogleResourceCandidate(alias, "", "vertex_ai", scope, false)
	if !ok || candidate.Vertex == nil {
		writeError(w, http.StatusBadRequest, "model_required", "Select a configured Vertex AI batch model")
		return
	}
	query := url.Values{}
	if limit := r.URL.Query().Get("limit"); limit != "" {
		query.Set("pageSize", limit)
	}
	if cursor := r.URL.Query().Get("page_token"); cursor != "" {
		query.Set("pageToken", cursor)
	}
	path := vertexBatchPath(candidate)
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	raw, ok := s.sendVertexBatch(w, r, candidate, http.MethodGet, path, nil)
	if !ok {
		return
	}
	var page struct {
		Jobs []vertexBatch `json:"batchPredictionJobs"`
		Next string        `json:"nextPageToken"`
	}
	if json.Unmarshal(raw, &page) != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Vertex batch list")
		return
	}
	admin := s.authorized(r)
	var owners map[string]string
	scopes := s.containerOwnerScopes(r, admin)
	if !admin && len(page.Jobs) > 0 {
		ids := make([]string, 0, len(page.Jobs))
		for _, job := range page.Jobs {
			_, id, _ := strings.Cut(job.Name, "/batchPredictionJobs/")
			ids = append(ids, "vertex_batch:"+id)
		}
		var err error
		owners, err = s.config.ContainerOwners.Read(r.Context(), candidate.DeploymentID, ids)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not check Vertex batch ownership")
			return
		}
	}
	data := make([]map[string]any, 0, len(page.Jobs))
	for _, job := range page.Jobs {
		_, id, _ := strings.Cut(job.Name, "/batchPredictionJobs/")
		if !admin && !slices.Contains(scopes, owners["vertex_batch:"+id]) {
			continue
		}
		if response := job.response(candidate); response != nil {
			data = append(data, response)
		}
	}
	var first, last, next any
	if len(data) != 0 {
		first, last = data[0]["id"], data[len(data)-1]["id"]
	}
	if page.Next != "" {
		next = page.Next
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data, "first_id": first, "last_id": last, "has_more": next != nil, "next_page_token": next})
}

func (s *Server) sendVertexBatch(w http.ResponseWriter, r *http.Request, candidate Candidate, method, path string, payload map[string]any) ([]byte, bool) {
	base := strings.TrimSuffix(strings.TrimRight(candidate.APIBase, "/"), "/v1")
	var data []byte
	if payload != nil {
		data, _ = json.Marshal(payload)
	}
	upstream, err := newGoogleResourceRequest(r, candidate, base+path, data)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not prepare Vertex batch request")
		return nil, false
	}
	upstream.Method = method
	upstream.Header.Set("Content-Type", "application/json")
	select {
	case s.sem <- struct{}{}:
	case <-r.Context().Done():
		return nil, false
	}
	defer func() { <-s.sem }()
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(upstream)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not reach Vertex AI")
		return nil, false
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes+1))
	if err != nil || int64(len(raw)) > s.config.MaxBodyBytes || response.StatusCode/100 == 3 {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Vertex batch response")
		return nil, false
	}
	if response.StatusCode/100 != 2 {
		writeRawResponse(w, response.StatusCode, response.Header, raw)
		return nil, false
	}
	return raw, true
}
