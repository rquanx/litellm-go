package runtime

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
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
	"anthropic":     {"https://api.anthropic.com", "ANTHROPIC_API_BASE", []string{"ANTHROPIC_API_KEY"}},
	"azure":         {"", "AZURE_API_BASE", []string{"AZURE_API_KEY"}},
	"vllm":          {"", "VLLM_API_BASE", []string{"VLLM_API_KEY"}},
}

func (s *Server) serveProviderPassThrough(w http.ResponseWriter, r *http.Request) bool {
	provider, suffix, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	prefix := "/" + provider
	if provider == "bedrock" {
		return s.serveBedrockPassThrough(w, r, suffix)
	}
	if provider == "openai_passthrough" {
		provider = "openai"
	}
	if provider == "azure_ai" {
		provider = "azure"
	}
	config, ok := providerPassThroughRoutes[provider]
	if !ok || suffix == "" || !slices.Contains([]string{"GET", "POST", "PUT", "DELETE", "PATCH"}, r.Method) {
		return false
	}
	token := ProxyCredential(r)
	if provider == "gemini" && r.URL.Query().Get("key") != "" {
		token = r.URL.Query().Get("key")
	}
	var scope []string
	if s.config.Token != "" && token != s.config.Token {
		var valid bool
		if s.config.AuthorizeKey != nil {
			scope, valid = s.config.AuthorizeKey(token)
		}
		if !valid {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Runtime token required")
			return true
		}
		if provider != "vllm" && !modelAllowed(scope, "*") {
			writeError(w, http.StatusForbidden, "model_not_allowed", "This provider route requires unrestricted model access")
			return true
		}
	}
	var key string
	deployment, configured := s.passThroughDeployment(provider)
	if provider == "vllm" {
		selected, valid := s.vllmPassThroughDeployment(w, r, suffix, scope)
		if !valid {
			return true
		}
		if selected.DeploymentID != "" {
			deployment, configured = selected, true
		}
	}
	if configured {
		key = deployment.APIKey
		if key == "" {
			key = os.Getenv(deployment.CredentialRef)
		}
	}
	for _, variable := range config.keys {
		if key != "" {
			break
		}
		key = os.Getenv(variable)
	}
	if key == "" && deployment.AzureADToken == "" && provider != "vllm" {
		writeError(w, http.StatusBadGateway, "upstream_error", "Provider credentials are missing")
		return true
	}
	base := os.Getenv(config.baseEnv)
	if configured {
		base = deployment.APIBase
	}
	if base == "" && provider == "anthropic" {
		base = os.Getenv("ANTHROPIC_BASE_URL")
	}
	if base == "" {
		base = config.base
	}
	target, err := url.Parse(base)
	if err != nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") {
		writeError(w, http.StatusBadGateway, "upstream_error", "Invalid pass-through provider base")
		return true
	}
	route := PassThroughRoute{Path: prefix, Target: *target, MergeQuery: true, Headers: make(http.Header)}
	if provider == "vllm" && strings.HasSuffix(strings.TrimRight(target.Path, "/"), "/v1") && strings.HasPrefix(suffix, "v1/") {
		route.Target.Path = strings.TrimSuffix(strings.TrimRight(target.Path, "/"), "/v1")
		route.Target.RawPath = ""
	}
	switch provider {
	case "gemini":
		route.UpstreamQuery = url.Values{"key": {key}}
	case "cursor":
		route.Headers.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(key+":")))
	case "assemblyai", "eu.assemblyai":
		route.Headers.Set("Authorization", key)
	case "anthropic":
		route.ForwardHeaders = true
		route.Headers.Set("X-Api-Key", key)
		if r.Header.Get("Anthropic-Version") == "" {
			route.Headers.Set("Anthropic-Version", "2023-06-01")
		}
	case "azure":
		if deployment.AzureADToken != "" {
			route.Headers.Set("Authorization", "Bearer "+deployment.AzureADToken)
		} else {
			route.Headers.Set("Api-Key", key)
			route.Headers.Set("Authorization", "Bearer "+key)
		}
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
		if key != "" {
			route.Headers.Set("Authorization", "Bearer "+key)
		}
	}
	s.forwardPassThrough(w, r, route)
	return true
}

func (s *Server) vllmPassThroughDeployment(w http.ResponseWriter, r *http.Request, suffix string, scope []string) (Candidate, bool) {
	unrestricted := modelAllowed(scope, "*")
	if r.Method != http.MethodPost {
		if !unrestricted {
			writeError(w, http.StatusForbidden, "model_not_allowed", "This vLLM route requires unrestricted model access")
		}
		return Candidate{}, unrestricted
	}
	body, err := readBody(r, s.config.MaxBodyBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large")
		return Candidate{}, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var fields map[string]json.RawMessage
	var alias string
	if json.Unmarshal(body, &fields) == nil {
		json.Unmarshal(fields["model"], &alias)
	}
	s.mu.RLock()
	route, exists := s.configuredRoutes[alias]
	s.mu.RUnlock()
	if !exists {
		if !unrestricted {
			writeError(w, http.StatusForbidden, "model_not_allowed", "A configured vLLM model is required")
		}
		return Candidate{}, unrestricted
	}
	operation := strings.TrimPrefix(suffix, "v1/")
	if !modelAllowed(scope, alias) || (!unrestricted &&
		!slices.Contains([]string{"chat/completions", "completions", "responses", "embeddings", "rerank", "tokenize", "detokenize"}, operation)) {
		writeError(w, http.StatusForbidden, "model_not_allowed", "This vLLM operation is unavailable to the key")
		return Candidate{}, false
	}
	for _, candidate := range route.Candidates {
		if !candidateAllowed(scope, candidate) || !isVLLMProvider(candidate.Provider) {
			continue
		}
		fields["model"], _ = json.Marshal(candidate.ModelID)
		forwarded, _ := json.Marshal(fields)
		r.Body = io.NopCloser(bytes.NewReader(forwarded))
		return candidate, true
	}
	writeError(w, http.StatusNotFound, "model_not_found", "No vLLM deployment for this model")
	return Candidate{}, false
}

func isVLLMProvider(provider string) bool {
	return provider == "vllm" || provider == "hosted_vllm"
}

func (s *Server) passThroughDeployment(provider string) (Candidate, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var selected Candidate
	for alias, route := range s.configuredRoutes {
		for _, candidate := range route.Candidates {
			if !candidate.UseInPassThrough || (candidate.RoutingGroup != "" && candidate.RoutingGroup != alias) ||
				(candidate.Provider != provider && (provider != "cohere" || candidate.Provider != "cohere_chat") &&
					(provider != "vllm" || !isVLLMProvider(candidate.Provider))) {
				continue
			}
			if selected.DeploymentID == "" || candidate.DeploymentID < selected.DeploymentID {
				selected = candidate
			}
		}
	}
	return selected, selected.DeploymentID != ""
}
