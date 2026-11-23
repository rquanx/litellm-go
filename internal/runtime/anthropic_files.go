package runtime

import (
	"encoding/json"
	"net/http"
	"time"
)

func normalizeAnthropicFiles(body []byte, method string) []byte {
	var response map[string]any
	if json.Unmarshal(body, &response) != nil || response == nil {
		return body
	}
	transform := func(file map[string]any) {
		file["object"] = "file"
		delete(file, "type")
		if method == http.MethodDelete {
			file["deleted"] = true
			return
		}
		if count := file["size_bytes"]; count != nil {
			file["bytes"] = count
			delete(file, "size_bytes")
		}
		if raw, _ := file["created_at"].(string); raw != "" {
			if date, err := time.Parse(time.RFC3339Nano, raw); err == nil {
				file["created_at"] = date.Unix()
			}
		}
		file["purpose"] = "messages"
	}
	if files, ok := response["data"].([]any); ok {
		response["object"] = "list"
		for _, raw := range files {
			if file, ok := raw.(map[string]any); ok {
				transform(file)
			}
		}
	} else {
		transform(response)
	}
	result, _ := json.Marshal(response)
	return result
}
