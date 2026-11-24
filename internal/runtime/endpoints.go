package runtime

import "strings"

type endpointSpec struct {
	path      string
	family    string
	streaming bool
}

var endpoints = map[string]endpointSpec{
	"responses":          {"/v1/responses", "text", true},
	"responses-compact":  {"/v1/responses/compact", "responses-compact", false},
	"chat-completions":   {"/v1/chat/completions", "text", true},
	"embeddings":         {"/v1/embeddings", "embeddings", false},
	"completions":        {"/v1/completions", "completions", true},
	"image-generation":   {"/v1/images/generations", "image-generation", true},
	"image-edit":         {"/v1/images/edits", "image-edit", true},
	"speech":             {"/v1/audio/speech", "speech", false},
	"transcription":      {"/v1/audio/transcriptions", "transcription", true},
	"translation":        {"/v1/audio/translations", "translation", false},
	"moderations":        {"/v1/moderations", "moderations", false},
	"rerank":             {"/v1/rerank", "rerank", false},
	"vector-stores":      {"/v1/vector_stores", "vector-stores", false},
	"videos":            {"/v1/videos", "videos", false},
	"ocr":                {"/v1/ocr", "ocr", false},
	"anthropic-messages": {"/v1/messages", "anthropic", true},
	"gemini-generate":    {"", "gemini", false},
	"gemini-stream":      {"", "gemini", true},
	"google-embeddings":  {"", "embeddings", false},
	"cohere-embeddings":  {"", "embeddings", false},
	"bedrock-converse":   {"", "bedrock", true},
	"bedrock-embeddings": {"", "embeddings", false},
}

// EndpointFromPath resolves the public path used in LiteLLM model_info.
func EndpointFromPath(path string) string {
	if path == "" {
		return ""
	}
	for name, spec := range endpoints {
		if spec.path == path {
			return name
		}
	}
	return ""
}

func extendedResourceEndpoint(endpoint string) bool {
	return endpoint == "responses-compact" || endpoint == "vector-stores" || endpoint == "videos"
}

func proxyAlias(path string) bool {
	switch path {
	case "/models", "/chat/completions", "/completions", "/embeddings", "/moderations",
		"/audio/speech", "/audio/transcriptions", "/images/generations", "/images/edits", "/rerank", "/ocr", "/responses", "/rag/query", "/rag/ingest",
		"/messages/count_tokens":
		return true
	}
	return strings.HasPrefix(path, "/responses/") || strings.HasPrefix(path, "/models/") ||
		assistantFamily("/v1"+path) != "" || isFineTuningPath("/v1"+path) || isVectorStorePath("/v1"+path) || isVideoPath("/v1"+path) || isContainerPath("/v1"+path) || resourceFamily("/v1"+path) != ""
}

func deploymentEndpoint(path string) (string, string) {
	for _, prefix := range []string{"/openai/deployments/", "/engines/"} {
		remainder, ok := strings.CutPrefix(path, prefix)
		if !ok {
			continue
		}
		for _, name := range []string{"chat-completions", "completions", "embeddings", "image-generation", "image-edit"} {
			if prefix == "/engines/" && (name == "image-generation" || name == "image-edit") {
				continue
			}
			suffix := strings.TrimPrefix(endpoints[name].path, "/v1")
			if alias, ok := strings.CutSuffix(remainder, suffix); ok && alias != "" {
				return alias, name
			}
		}
	}
	return "", ""
}

// CompatibleEndpoints reports whether the runtime can serve both endpoints from a route.
func CompatibleEndpoints(a, b string) bool {
	first, ok := endpoints[a]
	return ok && (first.family == endpoints[b].family || (isConversationEndpoint(a) && isConversationEndpoint(b)))
}

func isConversationEndpoint(endpoint string) bool {
	return isTextEndpoint(endpoint) || endpoint == "anthropic-messages" || isGeminiEndpoint(endpoint) || endpoint == "bedrock-converse"
}

func servesEndpoint(request, provider string) bool {
	if request == "gemini-count-tokens" {
		return isGeminiEndpoint(provider)
	}
	if request == "responses-compact" {
		return provider == "responses" || provider == "responses-compact"
	}
	if request == "anthropic-count-tokens" {
		return provider == "anthropic-messages" || provider == "bedrock-converse"
	}
	if isGeminiEndpoint(request) {
		return isGeminiEndpoint(provider)
	}
	return CompatibleEndpoints(request, provider) &&
		(request != "anthropic-messages" || provider == "anthropic-messages" || provider == "bedrock-converse")
}
