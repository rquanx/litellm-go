package runtime

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
)

type providerPassThrough struct {
	base, baseEnv string
	keys          []string
}

var providerPassThroughRoutes = map[string]providerPassThrough{
	"gemini":        {"https://generativelanguage.googleapis.com", "GEMINI_API_BASE", []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}},
	"cohere":        {"https://api.cohere.com", "COHERE_API_BASE", []string{"COHERE_API_KEY"}},
	"mistral":       {"https://api.mistral.ai", "MISTRAL_API_BASE", []string{"MISTRAL_API_KEY"}},
	"assemblyai":    {"https://api.assemblyai.com", "ASSEMBLYAI_API_BASE", []string{"ASSEMBLYAI_API_KEY"}},
	"eu.assemblyai": {"https://api.eu.assemblyai.com", "ASSEMBLYAI_EU_API_BASE", []string{"ASSEMBLYAI_EU_API_KEY", "ASSEMBLYAI_API_KEY"}},
	"cursor":        {"https://api.cursor.com", "CURSOR_API_BASE", []string{"CURSOR_API_KEY"}},
	"openai":        {"https://api.openai.com", "OPENAI_API_BASE", []string{"OPENAI_API_KEY"}},
}

func (s *Server) serveProviderPassThrough(w http.ResponseWriter, r *http.Request) bool {
	provider, suffix, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	prefix := "/" + provider
	if provider == "openai_passthrough" {
		provider = "openai"
	}
	config, ok := providerPassThroughRoutes[provider]
	if !ok || suffix == "" || !slices.Contains([]string{"GET", "POST", "PUT", "DELETE", "PATCH"}, r.Method) {
		return false
	}
	token := ProxyCredential(r)
	if provider == "gemini" && r.URL.Query().Get("key") != "" {
		token = r.URL.Query().Get("key")
	}
	if s.config.Token != "" && token != s.config.Token {
		var scope []string
		var valid bool
		if s.config.AuthorizeKey != nil {
			scope, valid = s.config.AuthorizeKey(token)
		}
		if !valid {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Runtime token required")
			return true
		}
		if !modelAllowed(scope, "*") {
			writeError(w, http.StatusForbidden, "model_not_allowed", "This provider route requires unrestricted model access")
			return true
		}
	}
	var key string
	for _, variable := range config.keys {
		if key = os.Getenv(variable); key != "" {
			break
		}
	}
	if key == "" {
		writeError(w, http.StatusBadGateway, "upstream_error", "Provider credentials are missing")
		return true
	}
	base := os.Getenv(config.baseEnv)
	if base == "" {
		base = config.base
	}
	target, err := url.Parse(base)
	if err != nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid pass-through provider base")
		return true
	}
	route := PassThroughRoute{Path: prefix, Target: *target, MergeQuery: true, Headers: make(http.Header)}
	switch provider {
	case "gemini":
		route.UpstreamQuery = url.Values{"key": {key}}
	case "cursor":
		route.Headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(key+":")))
	case "assemblyai", "eu.assemblyai":
		route.Headers.Set("Authorization", key)
	case "openai":
		route.Headers.Set("Authorization", "Bearer "+key)
		route.Headers.Set("Api-Key", key)
		if target.Hostname() == "api.openai.com" && !strings.HasPrefix(suffix, "v1/") && !strings.HasSuffix(target.Path, "/v1") {
			route.Target = *target.JoinPath("v1")
		}
		for _, segment := range strings.Split(suffix, "/") {
			if slices.Contains([]string{"assistants", "threads", "runs", "steps"}, segment) {
				route.Headers.Set("OpenAI-Beta", "assistants=v2")
				break
			}
		}
	default:
		route.Headers.Set("Authorization", "Bearer "+key)
	}
	s.forwardPassThrough(w, r, route)
	return true
}
