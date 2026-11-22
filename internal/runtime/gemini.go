package runtime

import (
	"net/http"
	"net/url"
	"strings"
)

func geminiMethod(path string) (alias, endpoint string) {
	path, ok := strings.CutPrefix(path, "/v1beta/models/")
	if !ok {
		path, ok = strings.CutPrefix(path, "/models/")
		if !ok {
			return "", ""
		}
	}
	alias, action, ok := strings.Cut(path, ":")
	if !ok || alias == "" {
		return "", ""
	}
	switch action {
	case "generateContent":
		return alias, "gemini-generate"
	case "streamGenerateContent":
		return alias, "gemini-stream"
	}
	return "", ""
}

func geminiUsage(payload map[string]any) any {
	raw, ok := payload["usageMetadata"].(map[string]any)
	if !ok {
		return nil
	}
	result := map[string]any{
		"input_tokens": raw["promptTokenCount"], "output_tokens": raw["candidatesTokenCount"], "total_tokens": raw["totalTokenCount"],
	}
	if cached := raw["cachedContentTokenCount"]; cached != nil {
		result["input_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	if thoughts := raw["thoughtsTokenCount"]; thoughts != nil {
		result["output_tokens_details"] = map[string]any{"reasoning_tokens": thoughts}
	}
	return result
}

func geminiURL(base, model, endpoint string) string {
	base = strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1beta")
	model = strings.TrimPrefix(model, "models/")
	target := base + "/v1beta/models/" + url.PathEscape(model)
	if endpoint == "gemini-stream" {
		return target + ":streamGenerateContent?alt=sse"
	}
	return target + ":generateContent"
}

func isGeminiEndpoint(endpoint string) bool {
	return endpoint == "gemini-generate" || endpoint == "gemini-stream"
}

func applyGeminiCredential(request *http.Request, key string) {
	if key != "" {
		request.Header.Set("x-goog-api-key", key)
	}
}
