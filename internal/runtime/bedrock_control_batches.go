package runtime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
)

func (s *Server) bedrockControlDeployment(r *http.Request, alias, region, id string, scope []string) (Candidate, int) {
	s.mu.RLock()
	var candidates []Candidate
	for name, route := range s.configuredRoutes {
		if (alias != "" && alias != name) || !modelAllowed(scope, name) {
			continue
		}
		for _, item := range route.Candidates {
			if item.Provider == "bedrock" && item.Bedrock != nil && candidateAllowed(scope, item) &&
				item.Endpoint == "bedrock-converse" && (region == "" || region == item.Bedrock.Region) {
				candidates = append(candidates, item)
			}
		}
	}
	s.mu.RUnlock()
	admin := s.authorized(r)
	ownerScopes := s.containerOwnerScopes(r, admin)
	var chosen Candidate
	for _, item := range candidates {
		if !admin && id != "" {
			owners, err := s.config.ContainerOwners.Read(r.Context(), item.DeploymentID, []string{id})
			if err != nil {
				return Candidate{}, http.StatusServiceUnavailable
			}
			if !slices.Contains(ownerScopes, owners[id]) {
				continue
			}
		}
		if chosen.ModelID != "" && (item.DeploymentID == "" || item.DeploymentID != chosen.DeploymentID) {
			return Candidate{}, http.StatusBadRequest
		}
		chosen = item
	}
	if chosen.ModelID == "" {
		return Candidate{}, http.StatusNotFound
	}
	return chosen, 0
}

func (s *Server) createBedrockControlBatch(w http.ResponseWriter, r *http.Request, alias string, scope []string, fields map[string]any) {
	candidate, status := s.bedrockControlDeployment(r, alias, "", "", scope)
	if status != 0 {
		writeError(w, status, "model_required", "Select one configured Bedrock batch model")
		return
	}
	bedrock := candidate.Bedrock
	input, _ := fields["input_file_id"].(string)
	uri, err := url.Parse(input)
	if err != nil || uri.Scheme != "s3" || uri.Host == "" || uri.Path == "" || uri.User != nil || uri.RawQuery != "" || uri.Fragment != "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "Bedrock batches require an S3 input object URI")
		return
	}
	if !s.authorized(r) {
		if _, err := bedrockS3Target(bedrock, uri); err == nil {
			owners, err := s.config.ContainerOwners.Read(r.Context(), candidate.DeploymentID, []string{input})
			if err != nil {
				writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not check Bedrock file ownership")
				return
			}
			if !slices.Contains(s.containerOwnerScopes(r, false), owners[input]) {
				writeError(w, http.StatusNotFound, "resource_not_found", "Unknown Bedrock input file")
				return
			}
		}
	}
	role, err := arn.Parse(bedrock.BatchRole)
	if err != nil || role.Service != "iam" || !strings.HasPrefix(role.Resource, "role/") {
		writeError(w, http.StatusBadRequest, "batch_role_required", "Configure aws_batch_role_arn for the selected Bedrock deployment")
		return
	}
	jobName := "litellm-" + newUUID()
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		digest := sha256.Sum256([]byte(s.containerOwnerScopes(r, s.authorized(r))[0] + "\x00" + candidate.DeploymentID + "\x00" + key))
		jobName = "litellm-" + hex.EncodeToString(digest[:20])
	}
	bucket := bedrock.BatchOutputBucket
	if bucket == "" {
		bucket = uri.Host
	}
	output := map[string]string{"s3Uri": "s3://" + bucket + "/litellm-batch-outputs/" + jobName + "/"}
	if bedrock.BatchEncryptionKey != "" {
		output["s3EncryptionKeyId"] = bedrock.BatchEncryptionKey
	}
	body := map[string]any{
		"modelId": candidate.ModelID, "jobName": jobName, "clientRequestToken": jobName,
		"roleArn":          bedrock.BatchRole,
		"inputDataConfig":  map[string]any{"s3InputDataConfig": map[string]string{"s3Uri": input}},
		"outputDataConfig": map[string]any{"s3OutputDataConfig": output},
	}
	window, _ := fields["completion_window"].(string)
	if window == "" || window == "24h" {
		body["timeoutDurationInHours"] = 24
	} else {
		writeError(w, http.StatusBadRequest, "invalid_request", "Bedrock batch completion_window must be 24h")
		return
	}
	raw, ok := s.sendBedrockControlBatch(w, r, candidate, http.MethodPost, "/model-invocation-job", body)
	if !ok {
		return
	}
	var created struct {
		ARN string `json:"jobArn"`
	}
	job, valid := arn.ARN{}, false
	if json.Unmarshal(raw, &created) == nil {
		job, valid = bedrockControlARN(created.ARN)
	}
	if !valid || job.Region != bedrock.Region {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Bedrock batch ARN")
		return
	}
	if !s.authorized(r) {
		owner := s.containerOwnerScopes(r, false)[0]
		if err := s.config.ContainerOwners.Record(r.Context(), candidate.DeploymentID, created.ARN, owner); err != nil {
			writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not record Bedrock batch ownership")
			return
		}
	}
	endpoint, _ := fields["endpoint"].(string)
	if endpoint == "" {
		endpoint = "/v1/chat/completions"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": created.ARN, "object": "batch", "status": "validating", "input_file_id": input,
		"endpoint": endpoint, "completion_window": "24h", "created_at": time.Now().Unix(),
		"metadata": fields["metadata"],
	})
}

func bedrockControlARN(id string) (arn.ARN, bool) {
	job, err := arn.Parse(id)
	name, found := strings.CutPrefix(job.Resource, "model-invocation-job/")
	return job, err == nil && job.Service == "bedrock" && job.Region != "" && found && validResourceSegment(name)
}

func (s *Server) bedrockControlBatchResource(w http.ResponseWriter, r *http.Request, alias, provider string, scope []string) bool {
	raw, ok := strings.CutPrefix(r.URL.EscapedPath(), "/v1/batches/")
	if !ok || (provider != "" && provider != "bedrock") {
		return false
	}
	id, err := url.PathUnescape(raw)
	if err != nil {
		return false
	}
	job, valid := bedrockControlARN(id)
	if !valid {
		return false
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Bedrock control-plane batch retrieval requires GET")
		return true
	}
	candidate, status := s.bedrockControlDeployment(r, alias, job.Region, id, scope)
	if status != 0 {
		writeError(w, status, "resource_not_found", "Could not select the Bedrock batch deployment")
		return true
	}
	rawResult, ok := s.sendBedrockControlBatch(w, r, candidate, http.MethodGet, "/model-invocation-job/"+url.PathEscape(id), nil)
	if !ok {
		return true
	}
	var state struct {
		ARN      string          `json:"jobArn"`
		Name     string          `json:"jobName"`
		Model    string          `json:"modelId"`
		Status   string          `json:"status"`
		Message  string          `json:"message"`
		Submit   json.RawMessage `json:"submitTime"`
		Modified json.RawMessage `json:"lastModifiedTime"`
		End      json.RawMessage `json:"endTime"`
		Input    struct {
			S3 struct {
				URI string `json:"s3Uri"`
			} `json:"s3InputDataConfig"`
		} `json:"inputDataConfig"`
		Output struct {
			S3 struct {
				URI string `json:"s3Uri"`
			} `json:"s3OutputDataConfig"`
		} `json:"outputDataConfig"`
	}
	if json.Unmarshal(rawResult, &state) != nil || state.ARN != id {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Bedrock batch response")
		return true
	}
	normalized := map[string]string{"Submitted": "validating", "Validating": "validating", "Scheduled": "validating",
		"Completed": "completed", "PartiallyCompleted": "completed", "Stopping": "cancelling", "Stopped": "cancelled",
		"Failed": "failed", "Expired": "expired"}[state.Status]
	if normalized == "" {
		normalized = "in_progress"
	}
	output := ""
	if file, parseErr := url.Parse(state.Input.S3.URI); state.Output.S3.URI != "" && parseErr == nil && file.Path != "" && !strings.HasSuffix(file.Path, "/") {
		output = strings.TrimRight(state.Output.S3.URI, "/") + "/" + path.Base(job.Resource) + "/" + path.Base(file.Path) + ".out"
	}
	created, _ := bedrockControlTimestamp(state.Submit)
	result := map[string]any{
		"id": id, "object": "batch", "status": normalized, "completion_window": "24h", "endpoint": "/v1/chat/completions",
		"created_at": created, "input_file_id": state.Input.S3.URI, "output_file_id": nil,
		"request_counts": map[string]int{"total": 0, "completed": 0, "failed": 0},
		"metadata": map[string]string{"job_arn": id, "job_name": state.Name, "model_arn": state.Model, "failure_message": state.Message,
			"input_s3_uri": state.Input.S3.URI, "output_s3_uri": state.Output.S3.URI, "output_file_uri": output},
	}
	if modified, ok := bedrockControlTimestamp(state.Modified); ok {
		result["in_progress_at"] = modified
	}
	if normalized == "completed" && output != "" {
		result["output_file_id"] = output
		if !s.authorized(r) {
			uri, err := url.Parse(output)
			if err == nil {
				_, err = bedrockS3Target(candidate.Bedrock, uri)
			}
			if err == nil {
				owners, lookupErr := s.config.ContainerOwners.Read(r.Context(), candidate.DeploymentID, []string{id})
				owner := owners[id]
				if lookupErr != nil || owner == "" || s.config.ContainerOwners.Record(r.Context(), candidate.DeploymentID, output, owner) != nil {
					writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not record Bedrock output ownership")
					return true
				}
			}
		}
	}
	if at := map[string]string{"completed": "completed_at", "failed": "failed_at", "cancelled": "cancelled_at", "expired": "expired_at"}[normalized]; at != "" {
		if finished, ok := bedrockControlTimestamp(state.End); ok {
			result[at] = finished
		}
	}
	writeJSON(w, http.StatusOK, result)
	return true
}

func bedrockControlTimestamp(value json.RawMessage) (int64, bool) {
	if len(value) == 0 || string(value) == "null" {
		return 0, false
	}
	if value[0] == '"' {
		var parsed time.Time
		if err := json.Unmarshal(value, &parsed); err == nil {
			return parsed.Unix(), true
		}
		return 0, false
	}
	var seconds float64
	if err := json.Unmarshal(value, &seconds); err != nil || seconds > float64(1<<62) || seconds < -float64(1<<62) {
		return 0, false
	}
	return int64(seconds), true
}

func (s *Server) sendBedrockControlBatch(w http.ResponseWriter, r *http.Request, candidate Candidate, method, suffix string, body map[string]any) ([]byte, bool) {
	control := candidate.Bedrock.ControlBase
	if control == "" {
		region := candidate.Bedrock.Region
		domain := "amazonaws.com"
		if strings.HasPrefix(region, "cn-") {
			domain = "amazonaws.com.cn"
		}
		control = "https://bedrock." + region + "." + domain
	}
	return s.sendBedrockResource(w, r, candidate, method, strings.TrimRight(control, "/")+suffix, body)
}

func (s *Server) sendBedrockResource(w http.ResponseWriter, r *http.Request, candidate Candidate, method, target string, body map[string]any) ([]byte, bool) {
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	upstream, err := http.NewRequestWithContext(r.Context(), method, target, bytes.NewReader(data))
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Bedrock resource URL")
		return nil, false
	}
	upstream.Header.Set("Content-Type", "application/json")
	if err := signAWSRequest(r.Context(), upstream, candidate.Bedrock, data, "bedrock"); err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not authorize the Bedrock resource request")
		return nil, false
	}
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
		writeError(w, http.StatusBadGateway, "upstream_error", "Bedrock resource request failed")
		return nil, false
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, s.config.MaxBodyBytes+1))
	if err != nil || int64(len(raw)) > s.config.MaxBodyBytes || response.StatusCode/100 == 3 {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Bedrock resource response")
		return nil, false
	}
	if response.StatusCode/100 != 2 {
		writeRawResponse(w, response.StatusCode, response.Header, raw)
		return nil, false
	}
	return raw, true
}
