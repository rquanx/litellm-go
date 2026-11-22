package runtime

import (
	"encoding/json"
	"strings"
)

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
