package runtime

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/oauth2"
)

type VertexConfig struct {
	Project       string
	Location      string
	TokenSource   oauth2.TokenSource
	Bucket        string
	StorageBase   string
	DiscoveryBase string
}

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
	case "countTokens":
		return alias, "gemini-count-tokens"
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
		if count, ok := thoughts.(float64); ok {
			candidates, _ := raw["candidatesTokenCount"].(float64)
			result["output_tokens"] = candidates + count
		}
		result["output_tokens_details"] = map[string]any{"reasoning_tokens": thoughts}
	}
	return result
}

func geminiURL(base, model, endpoint string) string {
	base = strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1beta")
	model = strings.TrimPrefix(model, "models/")
	target := base + "/v1beta/models/" + url.PathEscape(model)
	if endpoint == "google-embeddings" {
		return target + ":batchEmbedContents"
	}
	if endpoint == "gemini-count-tokens" {
		return target + ":countTokens"
	}
	if endpoint == "gemini-stream" {
		return target + ":streamGenerateContent?alt=sse"
	}
	return target + ":generateContent"
}

func isGeminiEndpoint(endpoint string) bool {
	return endpoint == "gemini-generate" || endpoint == "gemini-stream"
}

func geminiTarget(candidate Candidate, endpoint string) string {
	if candidate.Vertex == nil {
		return geminiURL(candidate.APIBase, candidate.ModelID, endpoint)
	}
	base := strings.TrimSuffix(strings.TrimRight(candidate.APIBase, "/"), "/v1")
	vertex := candidate.Vertex
	model := strings.TrimPrefix(candidate.ModelID, "models/")
	project := base + "/v1/projects/" + url.PathEscape(vertex.Project) + "/locations/" + url.PathEscape(vertex.Location)
	target := project + "/publishers/google/models/" + url.PathEscape(model)
	if endpoint == "google-embeddings" {
		if VertexUsesEmbedContent(model) {
			return target + ":embedContent"
		}
		if id := strings.TrimPrefix(model, "bge/"); vertexNumericEndpoint(id) {
			return project + "/endpoints/" + id + ":predict"
		}
		return target + ":predict"
	}
	if endpoint == "gemini-count-tokens" {
		return target + ":countTokens"
	}
	if endpoint == "gemini-stream" {
		return target + ":streamGenerateContent?alt=sse"
	}
	return target + ":generateContent"
}

func applyGeminiCredential(request *http.Request, candidate Candidate, key string) error {
	if candidate.Vertex != nil {
		if key == "" && candidate.Vertex.TokenSource != nil {
			token, err := candidate.Vertex.TokenSource.Token()
			if err != nil {
				return errors.New("could not retrieve a Vertex AI access token")
			}
			key = token.AccessToken
		}
		if key == "" {
			return errors.New("Vertex AI requires an access token")
		}
		request.Header.Set("Authorization", "Bearer "+key)
		return nil
	}
	if key != "" {
		request.Header.Set("x-goog-api-key", key)
	}
	return nil
}
