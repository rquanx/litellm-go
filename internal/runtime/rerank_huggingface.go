package runtime

import (
	"encoding/json"
	"strings"
)

func huggingFaceRerankRequest(payload map[string]any) map[string]any {
	request := map[string]any{
		"query": payload["query"], "texts": payload["documents"], "raw_scores": false,
		"truncate": false, "truncation_direction": "Right",
	}
	if value, ok := payload["return_documents"].(bool); ok {
		request["return_text"] = value
	}
	if value := payload["top_n"]; value != nil {
		request["top_n"] = value
	}
	return request
}

func huggingFaceRerankResponse(body []byte, payload map[string]any, model string) []byte {
	var ranking []struct {
		Index *int     `json:"index"`
		Score *float64 `json:"score"`
		Text  string   `json:"text"`
	}
	if json.Unmarshal(body, &ranking) != nil || ranking == nil {
		return body
	}
	include, _ := payload["return_documents"].(bool)
	documents, _ := payload["documents"].([]any)
	results := make([]map[string]any, 0, len(ranking))
	for _, item := range ranking {
		if item.Index == nil || item.Score == nil {
			continue
		}
		index := *item.Index
		result := map[string]any{"index": index, "relevance_score": *item.Score}
		if include {
			text := item.Text
			if text == "" && index >= 0 && index < len(documents) {
				text = rerankDocumentText(documents[index])
			}
			if text != "" {
				result["document"] = map[string]string{"text": text}
			}
		}
		results = append(results, result)
	}
	var inputText strings.Builder
	query, _ := payload["query"].(string)
	inputText.WriteString(query)
	inputText.WriteByte(' ')
	for _, doc := range documents {
		inputText.WriteString(rerankDocumentText(doc))
		inputText.WriteByte(' ')
	}
	inputTokens, outputTokens := len(inputText.String())*4, max(len(ranking), 1)*10
	if codec, err := tokenizerForModel(model); err == nil {
		if count, err := codec.Count(inputText.String()); err == nil {
			inputTokens = count
		}
		if count, err := codec.Count(string(body)); err == nil {
			outputTokens = count
		}
	}
	encoded, _ := json.Marshal(map[string]any{"id": newUUID(), "results": results, "meta": map[string]any{
		"api_version":  map[string]string{"version": "1.0"},
		"billed_units": map[string]int{"search_units": 1},
		"tokens":       map[string]int{"input_tokens": inputTokens, "output_tokens": outputTokens},
	}})
	return encoded
}

func rerankDocumentText(document any) string {
	switch value := document.(type) {
	case string:
		return value
	case map[string]any:
		text, _ := value["text"].(string)
		return text
	default:
		return ""
	}
}
