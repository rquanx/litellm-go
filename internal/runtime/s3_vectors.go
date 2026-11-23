package runtime

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
)

func (s *Server) ragEmbeddings(w http.ResponseWriter, r *http.Request, scope []string, model string, input []string, options map[string]any) ([][]float64, bool) {
	if model == "" {
		model = "text-embedding-3-small"
	}
	payload := map[string]any{"model": model, "input": input}
	if dimensions := options["dimensions"]; dimensions != nil {
		payload["dimensions"] = dimensions
	}
	body, _ := json.Marshal(payload)
	stage, ok := s.ragStage(w, func(output http.ResponseWriter) {
		s.infer(output, ragSubrequest(r, http.MethodPost, "/v1/embeddings", body, false), "embeddings", model, scope)
	})
	if !ok {
		return nil, false
	}
	var result struct {
		Data []struct {
			Vector []float64 `json:"embedding"`
			Index  int       `json:"index"`
		} `json:"data"`
	}
	vectors := make([][]float64, len(input))
	if json.Unmarshal(stage.body.Bytes(), &result) == nil && len(result.Data) == len(input) {
		valid := true
		for _, item := range result.Data {
			if item.Index < 0 || item.Index >= len(vectors) || vectors[item.Index] != nil || len(item.Vector) == 0 {
				valid = false
				break
			}
			vectors[item.Index] = item.Vector
		}
		if valid {
			return vectors, true
		}
	}
	writeError(w, http.StatusBadGateway, "upstream_error", "The embedding deployment returned incomplete vectors")
	return nil, false
}

func (s *Server) sendS3VectorRequest(w http.ResponseWriter, r *http.Request, candidate Candidate, action string, payload map[string]any) ([]byte, bool) {
	base := candidate.Bedrock.VectorBase
	if base == "" {
		domain := "api.aws"
		if strings.HasPrefix(candidate.Bedrock.Region, "cn-") {
			domain = "api.amazonwebservices.com.cn"
		}
		base = "https://s3vectors." + candidate.Bedrock.Region + "." + domain
	}
	return s.sendAWSJSONResource(w, r, candidate, http.MethodPost, strings.TrimRight(base, "/")+"/"+action, "s3vectors", payload)
}

func (s *Server) searchS3Vectors(w http.ResponseWriter, r *http.Request, scope []string, alias, id string, fields map[string]any) {
	bucket, index, combined := strings.Cut(id, ":")
	if !combined {
		bucket, _ = fields["vector_bucket_name"].(string)
		index = id
	}
	query, valid := geminiVectorQuery(fields["query"])
	if !valid || !bedrockRAGBucket.MatchString(bucket) || !bedrockRAGBucket.MatchString(index) {
		writeError(w, http.StatusBadRequest, "invalid_request", "An S3 vector bucket, index and search query are required")
		return
	}
	region, _ := fields["aws_region_name"].(string)
	candidate, status := s.bedrockControlDeployment(r, alias, region, "", scope)
	if status != 0 {
		writeError(w, status, "model_required", "Select a configured AWS deployment for S3 Vectors")
		return
	}
	model, _ := fields["embedding_model"].(string)
	embeddings, ok := s.ragEmbeddings(w, r, scope, model, []string{query}, nil)
	if !ok {
		return
	}
	topK := fields["max_num_results"]
	if topK == nil {
		topK = 5
	}
	payload := map[string]any{"vectorBucketName": bucket, "indexName": index, "queryVector": map[string]any{"float32": embeddings[0]},
		"topK": topK, "returnDistance": true, "returnMetadata": true}
	raw, ok := s.sendS3VectorRequest(w, r, candidate, "QueryVectors", payload)
	if !ok {
		return
	}
	var response struct {
		Vectors []struct {
			Metadata map[string]any `json:"metadata"`
			Distance *float64       `json:"distance"`
		} `json:"vectors"`
	}
	if json.Unmarshal(raw, &response) != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid S3 Vectors search response")
		return
	}
	data := make([]map[string]any, 0, len(response.Vectors))
	for _, row := range response.Vectors {
		source, _ := row.Metadata["source_text"].(string)
		if source == "" {
			continue
		}
		chunk := "0"
		if value := row.Metadata["chunk_index"]; value != nil {
			chunk = fmt.Sprint(value)
		}
		filename, _ := row.Metadata["filename"].(string)
		if filename == "" {
			filename = "document-" + chunk
		}
		var score any
		if row.Distance != nil {
			score = math.Max(0, math.Min(1, 1-*row.Distance))
		}
		data = append(data, map[string]any{"file_id": "s3-vectors-chunk-" + chunk, "filename": filename, "score": score,
			"attributes": row.Metadata, "content": []map[string]string{{"type": "text", "text": source}}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "vector_store.search_results.page", "search_query": query, "data": data})
}
