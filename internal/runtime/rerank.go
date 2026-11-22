package runtime

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

func vertexRerankTarget(candidate Candidate) string {
	base := strings.TrimSuffix(strings.TrimRight(candidate.APIBase, "/"), "/v1")
	return base + "/v1/projects/" + url.PathEscape(candidate.Vertex.Project) + "/locations/global/rankingConfigs/default_ranking_config:rank"
}

func vertexRerankRequest(payload map[string]any) map[string]any {
	docs, _ := payload["documents"].([]any)
	records := make([]map[string]any, 0, len(docs))
	for index, document := range docs {
		record := map[string]any{"id": strconv.Itoa(index)}
		if text, ok := document.(string); ok {
			record["content"] = text
		} else if fields, ok := document.(map[string]any); ok {
			for _, field := range []string{"content", "title"} {
				if value, exists := fields[field]; exists {
					record[field] = value
				}
			}
			if record["content"] == nil {
				record["content"] = fields["text"]
			}
		}
		records = append(records, record)
	}
	request := map[string]any{"model": payload["model"], "query": payload["query"], "records": records}
	if limit, exists := payload["top_n"]; exists {
		request["topN"] = limit
	}
	if include, ok := payload["return_documents"].(bool); ok {
		request["ignoreRecordDetailsInResponse"] = !include
	}
	return request
}

func vertexRerankResponse(body []byte) []byte {
	var response struct {
		Records []struct {
			ID string `json:"id"`
			Score *float64 `json:"score"`
		} `json:"records"`
	}
	if json.Unmarshal(body, &response) != nil || response.Records == nil {
		return body
	}
	results := make([]map[string]any, 0, len(response.Records))
	for _, record := range response.Records {
		index, err := strconv.Atoi(record.ID)
		if err != nil {
			return body
		}
		score := 1.0
		if record.Score != nil {
			score = *record.Score
		}
		results = append(results, map[string]any{"index": index, "relevance_score": score})
	}
	value, err := json.Marshal(map[string]any{"results": results, "meta": map[string]any{
		"billed_units": map[string]int{"search_units": len(results)},
	}})
	if err != nil {
		return body
	}
	return value
}

func voyageRerankRequest(payload map[string]any) map[string]any {
	request := map[string]any{"model": payload["model"], "query": payload["query"],
		"documents": payload["documents"], "return_documents": true}
	if value, exists := payload["top_n"]; exists {
		request["top_k"] = value
	}
	if value, exists := payload["return_documents"]; exists {
		request["return_documents"] = value
	}
	return request
}

func voyageRerankResponse(body []byte) []byte {
	var response struct {
		ID string `json:"id"`
		Data []json.RawMessage `json:"data"`
		Usage struct {
			Total int `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &response) != nil || response.Data == nil {
		return body
	}
	value, err := json.Marshal(map[string]any{"id": response.ID, "results": response.Data, "meta": map[string]any{
		"billed_units": map[string]int{"total_tokens": response.Usage.Total},
		"tokens": map[string]int{"input_tokens": response.Usage.Total, "output_tokens": 0},
	}})
	if err != nil {
		return body
	}
	return value
}

func rerankTarget(base, provider string) string {
	base = strings.TrimRight(base, "/")
	path := "/v1/rerank"
	if provider == "cohere" || provider == "cohere_chat" {
		path = "/v2/rerank"
	}
	if strings.HasSuffix(base, path) {
		return base
	}
	base = strings.TrimSuffix(strings.TrimSuffix(base, "/v2"), "/v1")
	return base + path
}
