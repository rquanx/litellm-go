package runtime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
)

func (s *Server) bedrockAsyncBatchResource(w http.ResponseWriter, r *http.Request, alias, provider string, scope []string) bool {
	path, ok := strings.CutPrefix(r.URL.EscapedPath(), "/v1/batches/")
	if !ok {
		return false
	}
	id, err := url.PathUnescape(path)
	job, parseErr := arn.Parse(id)
	resource, async := strings.CutPrefix(job.Resource, "async-invoke/")
	if err != nil || parseErr != nil || job.Service != "bedrock" || !async || !validResourceSegment(resource) {
		return false
	}
	if provider != "" && provider != "bedrock" {
		writeError(w, http.StatusNotFound, "resource_not_found", "Unknown Bedrock batch")
		return true
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Bedrock async invocation status supports GET")
		return true
	}
	s.mu.RLock()
	var candidates []Candidate
	for name, route := range s.configuredRoutes {
		if (alias != "" && name != alias) || !modelAllowed(scope, name) {
			continue
		}
		for _, item := range route.Candidates {
			if item.Provider == "bedrock" && item.Bedrock != nil && item.Bedrock.Region == job.Region &&
				item.Endpoint == "bedrock-embeddings" && strings.HasPrefix(item.ModelID, "async_invoke/") && candidateAllowed(scope, item) {
				candidates = append(candidates, item)
			}
		}
	}
	s.mu.RUnlock()
	var chosen Candidate
	admin := s.authorized(r)
	ownerScopes := s.containerOwnerScopes(r, admin)
	for _, candidate := range candidates {
		if !admin {
			if s.config.ContainerOwners == nil || candidate.DeploymentID == "" {
				continue
			}
			owners, err := s.config.ContainerOwners.Read(r.Context(), candidate.DeploymentID, []string{id})
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not check Bedrock batch ownership")
				return true
			}
			if !slices.Contains(ownerScopes, owners[id]) {
				continue
			}
		}
		if chosen.ModelID != "" && (chosen.DeploymentID == "" || chosen.DeploymentID != candidate.DeploymentID) {
			writeError(w, http.StatusBadRequest, "model_required", "Select one Bedrock async embedding deployment")
			return true
		}
		chosen = candidate
	}
	if chosen.ModelID == "" {
		writeError(w, http.StatusNotFound, "resource_not_found", "Unknown Bedrock batch")
		return true
	}
	s.readBedrockAsyncBatch(w, r, chosen, id)
	return true
}

func (s *Server) readBedrockAsyncBatch(w http.ResponseWriter, r *http.Request, candidate Candidate, id string) {
	target := strings.TrimRight(candidate.APIBase, "/") + "/async-invoke/" + url.PathEscape(id)
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Bedrock batch URL")
		return
	}
	for name, value := range candidate.Headers {
		upstream.Header.Set(name, value)
	}
	upstream.Header.Set("Content-Type", "application/json")
	key := candidate.APIKey
	if key == "" {
		key = os.Getenv(candidate.CredentialRef)
	}
	if err := authorizeBedrock(r.Context(), upstream, candidate, nil, key); err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not authorize the Bedrock batch status request")
		return
	}
	select {
	case s.sem <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	defer func() { <-s.sem }()
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(upstream)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not retrieve batch status from Bedrock")
		return
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes+1))
	if err != nil || int64(len(raw)) > s.config.MaxBodyBytes || response.StatusCode/100 == 3 {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not read the Bedrock batch response")
		return
	}
	if response.StatusCode/100 != 2 {
		writeRawResponse(w, response.StatusCode, response.Header, raw)
		return
	}
	var status struct {
		ID       string `json:"invocationArn"`
		Model    string `json:"modelArn"`
		Status   string `json:"status"`
		Failure  string `json:"failureMessage"`
		Submit   string `json:"submitTime"`
		Modified string `json:"lastModifiedTime"`
		End      string `json:"endTime"`
		Output   struct {
			S3 struct {
				URI string `json:"s3Uri"`
			} `json:"s3OutputDataConfig"`
		} `json:"outputDataConfig"`
	}
	if json.Unmarshal(raw, &status) != nil || status.ID != id || status.Status == "" {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Bedrock batch status")
		return
	}
	timestamp := func(value string) any {
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return parsed.Unix()
		}
		return nil
	}
	created := timestamp(status.Submit)
	if created == nil {
		created = time.Now().Unix()
	}
	counts := map[string]int{"total": 1, "completed": 0, "failed": 0}
	result := map[string]any{
		"id": id, "object": "batch", "endpoint": "/v1/embeddings", "input_file_id": "",
		"completion_window": "24h", "created_at": created, "request_counts": counts,
		"metadata": map[string]string{"output_file_id": status.Output.S3.URI, "failure_message": status.Failure, "model_arn": status.Model},
	}
	switch strings.ToLower(status.Status) {
	case "inprogress", "in_progress":
		result["status"] = "in_progress"
		result["in_progress_at"] = timestamp(status.Modified)
	case "completed":
		result["status"] = "completed"
		result["completed_at"] = timestamp(status.End)
		counts["completed"] = 1
	default:
		result["status"] = "failed"
		result["failed_at"] = timestamp(status.End)
		counts["failed"] = 1
	}
	writeJSON(w, http.StatusOK, result)
}
