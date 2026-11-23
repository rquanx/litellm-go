package runtime

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
)

var bedrockKnowledgeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,47}$`)

func (s *Server) createBedrockKnowledge(w http.ResponseWriter, r *http.Request, candidate Candidate, base, name string, options map[string]any) (string, string, bool) {
	if !s.authorized(r) {
		writeError(w, http.StatusForbidden, "model_not_allowed", "Creating a Bedrock knowledge base requires the proxy master key")
		return "", "", false
	}
	if name == "" {
		name = "litellm-kb-" + newUUID()[:8]
	}
	role, _ := options["knowledge_base_role_arn"].(string)
	collection, _ := options["opensearch_collection_arn"].(string)
	bucket, _ := options["s3_bucket"].(string)
	prefix, _ := options["s3_prefix"].(string)
	if prefix == "" {
		prefix = "data/"
	}
	model, _ := options["embedding_model"].(string)
	if model == "" {
		model = "amazon.titan-embed-text-v2:0"
	}
	if !validResourceSegment(model) {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid Bedrock embedding model")
		return "", "", false
	}
	index, _ := options["opensearch_index_name"].(string)
	if index == "" {
		index = "bedrock-kb-index"
	}
	if !bedrockKnowledgeName.MatchString(name) || (bucket != "" && !bedrockRAGBucket.MatchString(bucket)) ||
		!validBedrockRAGKey(strings.TrimRight(prefix, "/")+"/document", nil) ||
		!bedrockRAGBucket.MatchString(index) {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid Bedrock knowledge base name, bucket, index or data source prefix")
		return "", "", false
	}
	if role == "" && collection == "" {
		dimension, valid := ragChunkOption(options, "dimension", 1024, 4096)
		if !valid || dimension == 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "Invalid Bedrock vector dimensions")
			return "", "", false
		}
		var ok bool
		role, collection, bucket, ok = s.provisionBedrockRAGInfrastructure(w, r, candidate, options, prefix, model, index)
		if !ok {
			return "", "", false
		}
	}
	roleARN, roleErr := arn.Parse(role)
	collectionARN, collectionErr := arn.Parse(collection)
	region := candidate.Bedrock.Region
	if bucket == "" || roleErr != nil || roleARN.Service != "iam" || !strings.HasPrefix(roleARN.Resource, "role/") ||
		collectionErr != nil || collectionARN.Service != "aoss" || collectionARN.Region != region ||
		!strings.HasPrefix(collectionARN.Resource, "collection/") || collectionARN.Partition != roleARN.Partition ||
		collectionARN.AccountID == "" || collectionARN.AccountID != roleARN.AccountID {
		writeError(w, http.StatusBadRequest, "invalid_request", "Provisioning requires an existing IAM role, regional OpenSearch collection, index and S3 bucket")
		return "", "", false
	}
	base = strings.TrimRight(base, "/") + "/knowledgebases/"
	body := map[string]any{
		"clientToken": newUUID(), "name": name, "roleArn": role,
		"knowledgeBaseConfiguration": map[string]any{"type": "VECTOR", "vectorKnowledgeBaseConfiguration": map[string]string{
			"embeddingModelArn": "arn:" + roleARN.Partition + ":bedrock:" + region + "::foundation-model/" + model,
		}},
		"storageConfiguration": map[string]any{"type": "OPENSEARCH_SERVERLESS", "opensearchServerlessConfiguration": map[string]any{
			"collectionArn": collection, "vectorIndexName": index, "fieldMapping": map[string]string{
				"metadataField": "AMAZON_BEDROCK_METADATA", "textField": "AMAZON_BEDROCK_TEXT_CHUNK", "vectorField": "bedrock-knowledge-base-default-vector",
			},
		}},
	}
	raw, ok := s.sendBedrockResource(w, r, candidate, http.MethodPut, base, body)
	if !ok {
		return "", "", false
	}
	var result struct {
		Knowledge struct {
			ID     string `json:"knowledgeBaseId"`
			Status string `json:"status"`
		} `json:"knowledgeBase"`
	}
	if json.Unmarshal(raw, &result) != nil || !bedrockRecordID.MatchString(result.Knowledge.ID) {
		writeError(w, http.StatusBadGateway, "upstream_error", "Bedrock returned no new knowledge base identifier")
		return "", "", false
	}
	id := result.Knowledge.ID
	path := base + id
	for attempt := 0; result.Knowledge.Status != "ACTIVE"; attempt++ {
		if attempt >= 30 || result.Knowledge.Status == "FAILED" || result.Knowledge.Status == "DELETING" {
			writeError(w, http.StatusBadGateway, "upstream_error", "Bedrock knowledge base "+id+" did not become active")
			return "", "", false
		}
		if attempt > 0 {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
				return "", "", false
			}
		}
		raw, ok = s.sendBedrockResource(w, r, candidate, http.MethodGet, path, nil)
		if !ok {
			return "", "", false
		}
		result.Knowledge.Status = ""
		if json.Unmarshal(raw, &result) != nil || result.Knowledge.Status == "" {
			writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Bedrock knowledge base status")
			return "", "", false
		}
	}
	sourceBody := map[string]any{
		"clientToken": newUUID(), "name": name + "-s3-source",
		"dataSourceConfiguration": map[string]any{"type": "S3", "s3Configuration": map[string]any{
			"bucketArn": "arn:" + roleARN.Partition + ":s3:::" + bucket, "inclusionPrefixes": []string{prefix},
		}},
	}
	raw, ok = s.sendBedrockResource(w, r, candidate, http.MethodPut, path+"/datasources/", sourceBody)
	if !ok {
		return "", "", false
	}
	var source struct {
		Data struct {
			ID string `json:"dataSourceId"`
		} `json:"dataSource"`
	}
	if json.Unmarshal(raw, &source) != nil || !bedrockRecordID.MatchString(source.Data.ID) {
		writeError(w, http.StatusBadGateway, "upstream_error", "Bedrock returned no data source identifier for "+id)
		return "", "", false
	}
	return id, source.Data.ID, true
}
