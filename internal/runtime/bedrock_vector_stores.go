package runtime

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path"
	"strings"
)

func bedrockVectorFilter(filter map[string]any) map[string]any {
	operator, _ := filter["operator"].(string)
	if operator == "" {
		operator, _ = filter["type"].(string)
	}
	for _, compound := range []struct{ input, output string }{{"and", "andAll"}, {"or", "orAll"}} {
		raw, _ := filter[compound.input].([]any)
		if operator == compound.input {
			raw, _ = filter["filters"].([]any)
		}
		if len(raw) > 0 {
			children := make([]map[string]any, 0, len(raw))
			for _, value := range raw {
				if child, ok := value.(map[string]any); ok {
					children = append(children, bedrockVectorFilter(child))
				}
			}
			if len(children) == 1 {
				return children[0]
			}
			return map[string]any{compound.output: children}
		}
	}
	if operator == "" {
		return filter
	}
	if name := map[string]string{"eq": "equals", "ne": "notEquals", "gt": "greaterThan", "gte": "greaterThanOrEquals",
		"lt": "lessThan", "lte": "lessThanOrEquals", "nin": "notIn"}[operator]; name != "" {
		operator = name
	}
	return map[string]any{operator: map[string]any{"key": filter["key"], "value": filter["value"]}}
}

func (s *Server) searchBedrockKnowledgeBase(w http.ResponseWriter, r *http.Request, alias, id string, scope []string, fields map[string]any) {
	candidate, status := s.bedrockControlDeployment(r, alias, "", "", scope)
	if status != 0 {
		writeError(w, status, "model_required", "Select one configured Bedrock deployment")
		return
	}
	if !validResourceSegment(id) {
		writeError(w, http.StatusBadRequest, "invalid_request", "Invalid Bedrock knowledge base ID")
		return
	}
	query, ok := geminiVectorQuery(fields["query"])
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "A knowledge base query is required")
		return
	}
	payload := map[string]any{"retrievalQuery": map[string]string{"text": query}}
	settings := make(map[string]any)
	if raw, _ := fields["extra_body"].(map[string]any); raw != nil {
		for _, name := range []string{"retrievalConfiguration", "retrieval_configuration"} {
			if config, ok := raw[name].(map[string]any); ok {
				if existing, _ := config["vectorSearchConfiguration"].(map[string]any); existing != nil {
					settings = clone(existing)
					break
				}
			}
		}
	}
	if count := fields["max_num_results"]; count != nil {
		settings["numberOfResults"] = count
	}
	if filter, ok := fields["filters"].(map[string]any); ok {
		settings["filter"] = bedrockVectorFilter(filter)
	}
	if len(settings) > 0 {
		payload["retrievalConfiguration"] = map[string]any{"vectorSearchConfiguration": settings}
	}
	base := candidate.Bedrock.AgentBase
	if base == "" {
		region := candidate.Bedrock.Region
		domain := "amazonaws.com"
		if strings.HasPrefix(region, "cn-") {
			domain = "amazonaws.com.cn"
		}
		base = "https://bedrock-agent-runtime." + region + "." + domain
	}
	target := strings.TrimRight(base, "/") + "/knowledgebases/" + url.PathEscape(id) + "/retrieve"
	raw, ok := s.sendBedrockResource(w, r, candidate, http.MethodPost, target, payload)
	if !ok {
		return
	}
	var response struct {
		Results []struct {
			Content struct {
				Text *string `json:"text"`
			} `json:"content"`
			Metadata map[string]any `json:"metadata"`
			Score    *float64       `json:"score"`
		} `json:"retrievalResults"`
	}
	if json.Unmarshal(raw, &response) != nil {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid Bedrock knowledge base response")
		return
	}
	data := make([]map[string]any, 0, len(response.Results))
	for _, item := range response.Results {
		if item.Content.Text == nil {
			continue
		}
		metadata := item.Metadata
		if metadata == nil {
			metadata = map[string]any{}
		}
		id, _ := metadata["x-amz-bedrock-kb-source-uri"].(string)
		filename := id
		if id == "" {
			chunk, _ := metadata["x-amz-bedrock-kb-chunk-id"].(string)
			source, _ := metadata["x-amz-bedrock-kb-data-source-id"].(string)
			if chunk == "" {
				chunk = "unknown"
			}
			if source == "" {
				source = "unknown"
			}
			id, filename = "bedrock-kb-"+chunk, "bedrock-kb-document-"+source
		} else if uri, err := url.Parse(id); err == nil {
			filename = uri.Host
			if uri.Path != "" && uri.Path != "/" {
				filename = path.Base(uri.Path)
			}
		}
		data = append(data, map[string]any{"file_id": id, "filename": filename, "score": item.Score,
			"attributes": metadata, "content": []map[string]string{{"type": "text", "text": *item.Content.Text}}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "vector_store.search_results.page", "search_query": query, "data": data})
}
