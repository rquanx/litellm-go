package runtime

type endpointSpec struct {
	path      string
	family    string
	streaming bool
}

var endpoints = map[string]endpointSpec{
	"responses":          {"/v1/responses", "text", true},
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
	"anthropic-messages": {"/v1/messages", "anthropic", true},
	"gemini-generate":    {"", "gemini", false},
	"gemini-stream":      {"", "gemini", true},
	"bedrock-converse":   {"", "bedrock", true},
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

// CompatibleEndpoints reports whether the runtime can serve both endpoints from a route.
func CompatibleEndpoints(a, b string) bool {
	first, ok := endpoints[a]
	return ok && (first.family == endpoints[b].family || (isConversationEndpoint(a) && isConversationEndpoint(b)))
}

func isConversationEndpoint(endpoint string) bool {
	return isTextEndpoint(endpoint) || endpoint == "anthropic-messages" || isGeminiEndpoint(endpoint) || endpoint == "bedrock-converse"
}

func servesEndpoint(request, provider string) bool {
	if request == "anthropic-count-tokens" {
		return provider == "anthropic-messages"
	}
	if isGeminiEndpoint(request) {
		return isGeminiEndpoint(provider)
	}
	return CompatibleEndpoints(request, provider) &&
		(request != "anthropic-messages" || provider == "anthropic-messages")
}
