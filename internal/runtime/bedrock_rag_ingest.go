package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
)

var bedrockRAGBucket = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

func (s *Server) ingestBedrockDocument(w http.ResponseWriter, r *http.Request, scope []string, alias, id, name string, options map[string]any, input ragIngestInput) {
	if id == "" {
		id, _ = options["knowledge_base_id"].(string)
	}
	source, _ := options["data_source_id"].(string)
	if (id != "" && !bedrockRecordID.MatchString(id)) || (source != "" && (id == "" || !bedrockRecordID.MatchString(source))) {
		writeError(w, http.StatusBadRequest, "invalid_request", "Select an existing Bedrock knowledge base and a valid data source")
		return
	}
	if input.FileID == "" && len(input.file) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "Submit one file source")
		return
	}
	if len(input.file) != 0 && (!validResourceSegment(input.filename) || strings.ContainsAny(input.filename, "\r\n\x00")) {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid Bedrock document filename")
		return
	}
	wait, _ := options["wait_for_ingestion"].(bool)
	timeout := 300 * time.Second
	if value, exists := options["ingestion_timeout"]; exists && value != nil {
		seconds, ok := value.(float64)
		if !ok || seconds < 0 || seconds >= float64((1<<63-1)/int64(time.Second)) || seconds != float64(int64(seconds)) {
			writeError(w, http.StatusBadRequest, "invalid_request", "Invalid Bedrock ingestion timeout")
			return
		}
		timeout = time.Duration(seconds) * time.Second
	}
	region, _ := options["aws_region_name"].(string)
	candidate, status := s.bedrockControlDeployment(r, alias, region, "", scope)
	if status != 0 {
		writeError(w, status, "model_required", "Select one configured Bedrock deployment")
		return
	}
	base := candidate.Bedrock.AgentControlBase
	if base == "" {
		domain := "amazonaws.com"
		if strings.HasPrefix(candidate.Bedrock.Region, "cn-") {
			domain = "amazonaws.com.cn"
		}
		base = "https://bedrock-agent." + candidate.Bedrock.Region + "." + domain
	}
	if id == "" {
		var ok bool
		id, source, ok = s.createBedrockKnowledge(w, r, candidate, base, name, options)
		if !ok {
			return
		}
	}
	sourcesURL := strings.TrimRight(base, "/") + "/knowledgebases/" + id + "/datasources/"
	if source == "" {
		raw, ok := s.sendBedrockResource(w, r, candidate, http.MethodPost, sourcesURL, map[string]any{})
		if !ok {
			return
		}
		var result struct {
			Sources []struct {
				ID string `json:"dataSourceId"`
			} `json:"dataSourceSummaries"`
		}
		if json.Unmarshal(raw, &result) != nil || len(result.Sources) == 0 || !bedrockRecordID.MatchString(result.Sources[0].ID) {
			writeError(w, http.StatusBadGateway, "upstream_error", "Bedrock returned no usable data source")
			return
		}
		source = result.Sources[0].ID
	}
	sourceURL := sourcesURL + source
	raw, ok := s.sendBedrockResource(w, r, candidate, http.MethodGet, sourceURL, nil)
	if !ok {
		return
	}
	var detail struct {
		Source struct {
			Configuration struct {
				S3 struct {
					BucketARN string   `json:"bucketArn"`
					Prefixes  []string `json:"inclusionPrefixes"`
				} `json:"s3Configuration"`
			} `json:"dataSourceConfiguration"`
		} `json:"dataSource"`
	}
	if json.Unmarshal(raw, &detail) != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Bedrock data source response")
		return
	}
	s3 := detail.Source.Configuration.S3
	bucket, err := arn.Parse(s3.BucketARN)
	if err != nil || bucket.Service != "s3" || !bedrockRAGBucket.MatchString(bucket.Resource) {
		writeError(w, http.StatusBadGateway, "upstream_error", "Bedrock data source does not identify an S3 bucket")
		return
	}
	if requested, _ := options["s3_bucket"].(string); requested != "" && requested != bucket.Resource {
		writeError(w, http.StatusBadRequest, "invalid_request", "The bucket must match the Bedrock data source")
		return
	}
	if len(input.file) == 0 {
		writeRAGIngest(w, id, input.FileID)
		return
	}
	prefix, _ := options["s3_prefix"].(string)
	if prefix == "" {
		prefix = "data/"
		if len(s3.Prefixes) > 0 && s3.Prefixes[0] != "" {
			prefix = s3.Prefixes[0]
		}
	}
	key := strings.TrimRight(prefix, "/") + "/" + input.filename
	if !validBedrockRAGKey(key, s3.Prefixes) {
		writeError(w, http.StatusBadRequest, "invalid_request", "The document path must match the Bedrock data source prefix")
		return
	}
	object := &url.URL{Scheme: "s3", Host: bucket.Resource, Path: "/" + key}
	if !s.sendBedrockS3(w, r, candidate, bedrockS3URL(candidate.Bedrock, object), http.MethodPut, input.media, input.file) {
		return
	}
	jobsURL := sourceURL + "/ingestionjobs/"
	raw, ok = s.sendBedrockResource(w, r, candidate, http.MethodPut, jobsURL, map[string]any{"clientToken": newUUID()})
	if !ok {
		return
	}
	var job struct {
		Ingestion struct {
			ID     string `json:"ingestionJobId"`
			Status string `json:"status"`
		} `json:"ingestionJob"`
	}
	if json.Unmarshal(raw, &job) != nil || !bedrockRecordID.MatchString(job.Ingestion.ID) {
		writeError(w, http.StatusBadGateway, "upstream_error", "Bedrock returned no ingestion job ID")
		return
	}
	if wait && timeout > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		poll := r.WithContext(ctx)
		target := jobsURL + job.Ingestion.ID
		for ctx.Err() == nil {
			raw, ok := s.sendBedrockResource(w, poll, candidate, http.MethodGet, target, nil)
			if !ok {
				return
			}
			job.Ingestion.Status = ""
			if json.Unmarshal(raw, &job) != nil || job.Ingestion.Status == "" {
				writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Bedrock ingestion job response")
				return
			}
			if job.Ingestion.Status != "STARTING" && job.Ingestion.Status != "IN_PROGRESS" {
				break
			}
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
			}
		}
	}
	writeRAGIngest(w, id, key)
}

func validBedrockRAGKey(key string, prefixes []string) bool {
	if len(key) > 1024 || strings.ContainsAny(key, "\\\r\n\x00") {
		return false
	}
	for _, segment := range strings.Split(key, "/") {
		if !validResourceSegment(segment) {
			return false
		}
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return len(prefixes) == 0
}
