package runtime

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

func bedrockRerankRequest(payload map[string]any, region string) map[string]any {
	documents, _ := payload["documents"].([]any)
	sources := make([]map[string]any, 0, len(documents))
	for _, document := range documents {
		inline := map[string]any{"type": "JSON", "jsonDocument": document}
		if text, ok := document.(string); ok {
			inline = map[string]any{"type": "TEXT", "textDocument": map[string]any{"text": text}}
		}
		sources = append(sources, map[string]any{"type": "INLINE", "inlineDocumentSource": inline})
	}
	model, _ := payload["model"].(string)
	if !strings.HasPrefix(model, "arn:") {
		model = "arn:aws:bedrock:" + region + "::foundation-model/" + model
	}
	limit := any(len(documents))
	if top, ok := payload["top_n"].(float64); ok && top > 0 {
		limit = top
	}
	return map[string]any{
		"queries": []map[string]any{{"type": "TEXT", "textQuery": map[string]any{"text": payload["query"]}}},
		"sources": sources,
		"rerankingConfiguration": map[string]any{"type": "BEDROCK_RERANKING_MODEL", "bedrockRerankingConfiguration": map[string]any{
			"modelConfiguration": map[string]string{"modelArn": model}, "numberOfResults": limit,
		}},
	}
}

func bedrockRerankResponse(body []byte) []byte {
	var response struct {
		ID string `json:"id"`
		Results []struct {
			Index int `json:"index"`
			Score float64 `json:"relevanceScore"`
		} `json:"results"`
		Usage map[string]any `json:"usage"`
	}
	if json.Unmarshal(body, &response) != nil || response.Results == nil {
		return body
	}
	if response.ID == "" {
		response.ID = newUUID()
	}
	billed := response.Usage
	if billed == nil {
		billed = map[string]any{"search_units": 1}
	}
	results := make([]map[string]any, 0, len(response.Results))
	for _, result := range response.Results {
		results = append(results, map[string]any{"index": result.Index, "relevance_score": result.Score})
	}
	encoded, err := json.Marshal(map[string]any{"id": response.ID, "results": results, "meta": map[string]any{"billed_units": billed}})
	if err != nil {
		return body
	}
	return encoded
}

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

func rerankTarget(candidate Candidate) string {
	base := strings.TrimRight(candidate.APIBase, "/")
	provider := candidate.Provider
	switch provider {
	case "fireworks_ai":
		if strings.HasSuffix(base, "/rerank") {
			return base
		}
		if strings.HasSuffix(base, "/v1") {
			return base + "/rerank"
		}
		return base + "/inference/v1/rerank"
	case "deepinfra":
		base = strings.TrimSuffix(base, "/openai")
		model := strings.Split(candidate.ModelID, "/")
		for i, segment := range model {
			model[i] = url.PathEscape(segment)
		}
		return base + "/inference/" + strings.Join(model, "/")
	case "nvidia_nim":
		return nvidiaRerankTarget(base, candidate.ModelID)
	case "watsonx":
		version := candidate.APIVersion
		if version == "" {
			version = "2024-03-13"
		}
		return base + "/ml/v1/text/rerank?version=" + url.QueryEscape(version)
	case "azure_ai":
		if strings.HasSuffix(base, "/v1/rerank") || strings.HasSuffix(base, "/v2/rerank") {
			return base
		}
		if strings.HasSuffix(base, "/v1") || strings.HasSuffix(base, "/v2") {
			return base + "/rerank"
		}
	case "hosted_vllm", "infinity", "huggingface":
		if provider == "hosted_vllm" {
			if root, ok := strings.CutSuffix(base, "/v1/rerank"); ok {
				return root + "/rerank"
			}
		}
		if strings.HasSuffix(base, "/rerank") {
			return base
		}
		return base + "/rerank"
	}
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

func deepInfraRerankRequest(payload map[string]any) map[string]any {
	documents, _ := payload["documents"].([]any)
	request := map[string]any{"documents": payload["documents"]}
	if query, ok := payload["query"].(string); ok && query != "" {
		queries := make([]string, len(documents))
		for i := range queries {
			queries[i] = query
		}
		request["queries"] = queries
	}
	for _, key := range []string{"queries", "service_tier", "instruction", "webhook"} {
		if value := payload[key]; value != nil {
			request[key] = value
		}
	}
	return request
}

func deepInfraRerankResponse(body []byte) []byte {
	var response struct {
		ID     string    `json:"request_id"`
		Scores []float64 `json:"scores"`
		Tokens int       `json:"input_tokens"`
	}
	if json.Unmarshal(body, &response) != nil {
		return body
	}
	results := make([]map[string]any, len(response.Scores))
	for i, score := range response.Scores {
		results[i] = map[string]any{"index": i, "relevance_score": score}
	}
	if response.ID == "" {
		response.ID = newUUID()
	}
	encoded, _ := json.Marshal(map[string]any{"id": response.ID, "results": results, "meta": map[string]any{
		"billed_units": map[string]int{"total_tokens": response.Tokens},
		"tokens":       map[string]int{"input_tokens": response.Tokens, "output_tokens": 0},
	}})
	return encoded
}

func cohereStyleRerankResponse(body []byte, provider string) []byte {
	var response struct {
		ID      string           `json:"id"`
		Results []map[string]any `json:"results"`
		Usage   map[string]int   `json:"usage"`
	}
	if json.Unmarshal(body, &response) != nil {
		return body
	}
	if response.Results == nil {
		if provider != "infinity" {
			return body
		}
		response.Results = []map[string]any{}
	}
	for _, result := range response.Results {
		if provider == "infinity" {
			if document, ok := result["document"].(string); ok && document != "" {
				result["document"] = map[string]string{"text": document}
			} else {
				delete(result, "document")
			}
		}
	}
	units, tokens := map[string]int{}, map[string]int{}
	for _, key := range []string{"search_units", "total_tokens"} {
		if value, ok := response.Usage[key]; ok {
			units[key] = value
		}
	}
	if provider == "infinity" {
		tokens["input_tokens"] = response.Usage["prompt_tokens"]
		tokens["output_tokens"] = response.Usage["total_tokens"] - tokens["input_tokens"]
	} else {
		for _, key := range []string{"input_tokens", "output_tokens"} {
			if value, ok := response.Usage[key]; ok {
				tokens[key] = value
			}
		}
	}
	if response.ID == "" {
		response.ID = newUUID()
	}
	encoded, _ := json.Marshal(map[string]any{"id": response.ID, "results": response.Results,
		"meta": map[string]any{"billed_units": units, "tokens": tokens}})
	return encoded
}

func hostedRerankRequest(payload map[string]any, provider string) map[string]any {
	request := map[string]any{"model": payload["model"], "query": payload["query"],
		"documents": payload["documents"], "return_documents": true}
	for _, field := range []string{"top_n", "return_documents"} {
		if value := payload[field]; value != nil {
			request[field] = value
		}
	}
	if provider == "fireworks_ai" {
		model, _ := payload["model"].(string)
		request["model"] = "fireworks/" + strings.TrimPrefix(model, "fireworks/")
	} else if fields := payload["rank_fields"]; fields != nil {
		request["rank_fields"] = fields
	}
	return request
}

func hostedRerankResponse(body []byte, provider string) []byte {
	type result struct {
		Index    int             `json:"index"`
		Score    float64         `json:"relevance_score"`
		Document json.RawMessage `json:"document,omitempty"`
	}
	var response struct {
		ID      string   `json:"id"`
		Model   string   `json:"model"`
		Data    []result `json:"data"`
		Results []result `json:"results"`
		Usage   struct {
			Total      int `json:"total_tokens"`
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &response) != nil {
		return body
	}
	units := map[string]int{"total_tokens": response.Usage.Total}
	tokens := map[string]int{"input_tokens": response.Usage.Total}
	if provider == "fireworks_ai" {
		if len(response.Data) > 0 {
			response.Results = response.Data
		}
		units = map[string]int{"search_units": response.Usage.Total}
		tokens = map[string]int{"input_tokens": response.Usage.Prompt, "output_tokens": response.Usage.Completion}
		if response.ID == "" {
			response.ID = response.Model
		}
	}
	if response.Results == nil {
		return body
	}
	for i := range response.Results {
		document := &response.Results[i].Document
		var object map[string]any
		var value string
		if json.Unmarshal(*document, &object) == nil && len(object) > 0 {
			value, _ = object["text"].(string)
		} else if provider == "fireworks_ai" {
			_ = json.Unmarshal(*document, &value)
		}
		if value != "" || (provider == "hosted_vllm" && len(object) > 0) {
			*document, _ = json.Marshal(map[string]string{"text": value})
		} else {
			*document = nil
		}
	}
	if response.ID == "" {
		response.ID = newUUID()
	}
	encoded, _ := json.Marshal(map[string]any{"id": response.ID, "results": response.Results,
		"meta": map[string]any{"billed_units": units, "tokens": tokens}})
	return encoded
}
