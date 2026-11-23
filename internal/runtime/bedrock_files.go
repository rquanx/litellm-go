package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var bedrockRecordID = regexp.MustCompile(`^[A-Za-z0-9]{1,11}$`)

func bedrockS3Target(config *BedrockConfig, uri *url.URL) (string, error) {
	bucket, prefix, _ := strings.Cut(config.S3Bucket, "/")
	if uri == nil || uri.Scheme != "s3" || uri.User != nil || uri.RawQuery != "" || uri.Fragment != "" ||
		uri.Host == "" || (uri.Host != bucket && uri.Host != config.BatchOutputBucket) {
		return "", errors.New("S3 object is outside the configured buckets")
	}
	key := strings.TrimPrefix(uri.Path, "/")
	if key == "" || strings.Contains(key, "\\") {
		return "", errors.New("Invalid S3 object key")
	}
	for _, part := range strings.Split(key, "/") {
		if part == "." || part == ".." || part == "" {
			return "", errors.New("Invalid S3 object key segment")
		}
	}
	upload := key
	if prefix = strings.Trim(prefix, "/"); prefix != "" {
		var matched bool
		upload, matched = strings.CutPrefix(key, prefix+"/")
		if !matched {
			upload = ""
		}
	}
	outputBucket := config.BatchOutputBucket
	if outputBucket == "" {
		outputBucket = bucket
	}
	validUpload := uri.Host == bucket && (strings.HasPrefix(upload, "litellm-bedrock-files/") || strings.HasPrefix(upload, "litellm-bedrock-files-"))
	validOutput := uri.Host == outputBucket && strings.HasPrefix(key, "litellm-batch-outputs/")
	if !validUpload && !validOutput {
		return "", errors.New("S3 object is outside the managed file prefixes")
	}
	base := config.S3Endpoint
	if base == "" {
		region := config.S3Region
		if region == "" {
			region = config.Region
		}
		domain := "amazonaws.com"
		if strings.HasPrefix(region, "cn-") {
			domain = "amazonaws.com.cn"
		}
		base = "https://s3." + region + "." + domain
	}
	return strings.TrimRight(base, "/") + "/" + url.PathEscape(uri.Host) + uri.EscapedPath(), nil
}

func (s *Server) uploadBedrockFile(w http.ResponseWriter, r *http.Request, alias string, scope []string, form requestPayload) {
	candidate, status := s.bedrockControlDeployment(r, alias, "", "", scope)
	if status != 0 {
		writeError(w, status, "model_required", "Select one configured Bedrock file deployment")
		return
	}
	var file []byte
	var filename, purpose string
	for _, part := range form.form {
		if part.name == "purpose" {
			purpose = string(part.body)
			continue
		}
		if part.name != "file" {
			continue
		}
		_, params, _ := mime.ParseMediaType(part.header.Get("Content-Disposition"))
		if filename != "" || params["filename"] == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "Supply exactly one named upload")
			return
		}
		filename, file = params["filename"], part.body
	}
	if filename == "" || len(file) == 0 || purpose == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "A named file and purpose are required")
		return
	}
	key := "litellm-bedrock-files/" + newUUID()
	media := "application/octet-stream"
	if purpose == "batch" {
		var err error
		file, err = bedrockBatchJSONL(file, candidate)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_batch_file", err.Error())
			return
		}
		if int64(len(file)) > s.config.MaxBodyBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Converted Bedrock file exceeds the body limit")
			return
		}
		key = "litellm-bedrock-files-" + newUUID() + ".jsonl"
		media = "application/jsonl"
	}
	bucket, prefix, _ := strings.Cut(candidate.Bedrock.S3Bucket, "/")
	if prefix = strings.Trim(prefix, "/"); prefix != "" {
		key = prefix + "/" + key
	}
	uri := &url.URL{Scheme: "s3", Host: bucket, Path: "/" + key}
	if !s.forwardBedrockS3(w, r, candidate, uri, http.MethodPut, media, file) {
		return
	}
	id := uri.String()
	if !s.authorized(r) {
		if err := s.config.ContainerOwners.Record(r.Context(), candidate.DeploymentID, id, s.containerOwnerScopes(r, false)[0]); err != nil {
			writeError(w, http.StatusServiceUnavailable, "resource_error", "Could not record Bedrock file ownership")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "object": "file", "status": "uploaded",
		"filename": filename, "purpose": purpose, "created_at": time.Now().Unix(), "bytes": len(file)})
}

func bedrockBatchJSONL(file []byte, candidate Candidate) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	seen := make(map[string]bool)
	for line := range bytes.SplitSeq(file, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var item struct {
			ID         string          `json:"custom_id"`
			Record     string          `json:"recordId"`
			Body       map[string]any  `json:"body"`
			ModelInput json.RawMessage `json:"modelInput"`
		}
		if json.Unmarshal(line, &item) != nil {
			return nil, errors.New("Invalid JSONL record")
		}
		id := item.Record
		native := item.ModelInput
		if item.Body != nil {
			messages, _ := item.Body["messages"].([]any)
			if len(messages) == 0 {
				return nil, errors.New("Bedrock batch requests require messages")
			}
			id = item.ID
			if id == "" {
				id = fmt.Sprintf("CALL%07d", len(seen))
			}
			var err error
			if strings.Contains(candidate.ModelID, "anthropic.claude-") {
				native, err = bedrockClaudePayload(chatToAnthropicRequest(item.Body), candidate.ModelID, nil)
			} else if strings.Contains(candidate.ModelID, "amazon.nova") {
				native, err = json.Marshal(chatToBedrock(item.Body))
			} else {
				body := clone(item.Body)
				delete(body, "model")
				native, err = json.Marshal(body)
			}
			if err != nil {
				return nil, errors.New("Could not convert JSONL messages for Bedrock")
			}
		}
		if !bedrockRecordID.MatchString(id) || seen[id] || len(native) == 0 || native[0] != '{' {
			return nil, errors.New("Bedrock JSONL requires unique record IDs and model input objects")
		}
		seen[id] = true
		if err := encoder.Encode(map[string]any{"recordId": id, "modelInput": native}); err != nil {
			return nil, err
		}
	}
	if len(seen) == 0 {
		return nil, errors.New("Bedrock JSONL requires at least one record")
	}
	return buffer.Bytes(), nil
}

func (s *Server) bedrockFileContent(w http.ResponseWriter, r *http.Request, alias, provider string, scope []string) bool {
	escaped, ok := strings.CutPrefix(r.URL.EscapedPath(), "/v1/files/")
	if !ok || r.Method != http.MethodGet || (provider != "" && provider != "bedrock") {
		return false
	}
	escaped, ok = strings.CutSuffix(escaped, "/content")
	id, err := url.PathUnescape(escaped)
	if !ok || err != nil || !strings.HasPrefix(id, "s3://") {
		return false
	}
	candidate, status := s.bedrockControlDeployment(r, alias, "", id, scope)
	if status != 0 {
		writeError(w, status, "resource_not_found", "Unknown Bedrock file")
		return true
	}
	uri, _ := url.Parse(id)
	s.forwardBedrockS3(w, r, candidate, uri, http.MethodGet, "", nil)
	return true
}

func (s *Server) forwardBedrockS3(w http.ResponseWriter, r *http.Request, candidate Candidate, uri *url.URL, method, media string, data []byte) bool {
	target, err := bedrockS3Target(candidate.Bedrock, uri)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_file_id", err.Error())
		return false
	}
	upstream, err := http.NewRequestWithContext(r.Context(), method, target, bytes.NewReader(data))
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid S3 request URL")
		return false
	}
	if media != "" {
		upstream.Header.Set("Content-Type", media)
	}
	if err := signAWSRequest(r.Context(), upstream, candidate.Bedrock, data, "s3"); err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not authorize the S3 request")
		return false
	}
	select {
	case s.sem <- struct{}{}:
	case <-r.Context().Done():
		return false
	}
	defer func() { <-s.sem }()
	client := *s.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(upstream)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Could not reach S3")
		return false
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		writeError(w, http.StatusBadGateway, "upstream_error", "S3 rejected the Bedrock file operation")
		return false
	}
	if method == http.MethodGet {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, response.Body)
	}
	return true
}
