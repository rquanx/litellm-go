package runtime

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

func nvidiaRerankTarget(base, model string) string {
	if strings.HasPrefix(model, "ranking/") {
		if strings.HasSuffix(base, "/ranking") {
			return base
		}
		return strings.TrimSuffix(base, "/v1") + "/v1/ranking"
	}
	if strings.Contains(base, "/retrieval/") {
		return base
	}
	parts := strings.Split(model, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.TrimSuffix(base, "/v1") + "/v1/retrieval/" + strings.Join(parts, "/") + "/reranking"
}

func nvidiaPassages(payload map[string]any) []map[string]any {
	documents, _ := payload["documents"].([]any)
	passages := make([]map[string]any, 0, len(documents))
	for _, doc := range documents {
		var text any
		switch value := doc.(type) {
		case string:
			text = value
		case map[string]any:
			if field, exists := value["text"]; exists {
				text = field
			} else {
				raw, _ := json.Marshal(value)
				text = string(raw)
			}
		default:
			text = fmt.Sprint(value)
		}
		passages = append(passages, map[string]any{"text": text})
	}
	return passages
}

func nvidiaRerankRequest(payload map[string]any) map[string]any {
	model, _ := payload["model"].(string)
	request := map[string]any{
		"model":    strings.ReplaceAll(strings.TrimPrefix(model, "ranking/"), "_", "."),
		"query":    map[string]any{"text": payload["query"]},
		"passages": nvidiaPassages(payload),
	}
	if count := payload["top_n"]; count != nil {
		request["top_k"] = count
	}
	if count := payload["top_k"]; count != nil {
		request["top_k"] = count
	}
	if truncate, _ := payload["truncate"].(string); truncate == "NONE" || truncate == "END" {
		request["truncate"] = truncate
	}
	return request
}

func nvidiaRerankResponse(body []byte, payload map[string]any) []byte {
	var response struct {
		ID       string `json:"id"`
		Rankings []struct {
			Index int     `json:"index"`
			Logit float64 `json:"logit"`
		} `json:"rankings"`
		Usage struct {
			Total int `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &response) != nil {
		return body
	}
	passages := nvidiaPassages(payload)
	results := make([]map[string]any, 0, len(response.Rankings))
	for _, ranking := range response.Rankings {
		result := map[string]any{"index": ranking.Index, "relevance_score": ranking.Logit}
		if ranking.Index >= 0 && ranking.Index < len(passages) {
			result["document"] = passages[ranking.Index]
		}
		results = append(results, result)
	}
	if response.ID == "" {
		response.ID = newUUID()
	}
	total := response.Usage.Total
	if total <= 0 {
		total = len(results)
	}
	encoded, _ := json.Marshal(map[string]any{"id": response.ID, "results": results, "meta": map[string]any{
		"billed_units": map[string]int{"total_tokens": total},
	}})
	return encoded
}
