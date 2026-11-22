package proxyconfig

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/rquanx/minato-runtime/internal/runtime"
	"go.yaml.in/yaml/v3"
)

type configFile struct {
	Models  []deployment `yaml:"model_list"`
	General struct {
		MasterKey string `yaml:"master_key"`
	} `yaml:"general_settings"`
	Router struct {
		Fallbacks []map[string][]string `yaml:"fallbacks"`
		Timeout   int                  `yaml:"timeout"`
		Retries   int                  `yaml:"num_retries"`
	} `yaml:"router_settings"`
}

type deployment struct {
	Alias  string `yaml:"model_name" json:"model_name"`
	Params struct {
		Model   string `yaml:"model" json:"model"`
		APIBase string `yaml:"api_base" json:"api_base,omitempty"`
		APIKey  string `yaml:"api_key" json:"api_key,omitempty"`
		APIVersion string `yaml:"api_version" json:"api_version,omitempty"`
	} `yaml:"litellm_params" json:"litellm_params"`
	Info struct {
		ID        string   `yaml:"id" json:"id,omitempty"`
		Endpoints []string `yaml:"supported_endpoints" json:"supported_endpoints,omitempty"`
	} `yaml:"model_info" json:"model_info"`
}

type Config struct {
	Routes    []runtime.Route
	MasterKey string
	Timeout   time.Duration
	NumRetries int
	models    []deployment
	fallbacks []map[string][]string
}

func Load(reader io.Reader) (Config, error) {
	var file configFile
	decoder := yaml.NewDecoder(reader)
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		return Config{}, errors.New("invalid or unsupported LiteLLM YAML configuration")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("expected one LiteLLM YAML document")
	}
	if len(file.Models) == 0 || file.Router.Timeout < 0 || file.Router.Timeout > 86400 || file.Router.Retries < 0 || file.Router.Retries > 8 {
		return Config{}, errors.New("model_list is required and router settings must use a valid timeout and retry count between zero and eight")
	}
	config := Config{Timeout: time.Duration(file.Router.Timeout) * time.Second, NumRetries: file.Router.Retries, models: file.Models, fallbacks: file.Router.Fallbacks}
	var err error
	config.MasterKey, err = resolveEnv(file.General.MasterKey)
	if err != nil {
		return Config{}, fmt.Errorf("general_settings.master_key: %w", err)
	}
	for index := range config.models {
		model := &config.models[index]
		if model.Info.ID == "" {
			model.Info.ID = fmt.Sprintf("yaml:%s:%d", model.Alias, index)
		}
	}
	config.Routes, err = buildRoutes(config.models, config.fallbacks)
	if err != nil {
		return Config{}, err
	}
	return config, nil
}

func buildRoutes(models []deployment, configuredFallbacks []map[string][]string) ([]runtime.Route, error) {
	byAlias := make(map[string][]runtime.Candidate)
	var aliases []string
	for _, model := range models {
		candidate, err := model.candidate()
		if err != nil {
			return nil, fmt.Errorf("model %q: %w", model.Alias, err)
		}
		if _, exists := byAlias[model.Alias]; !exists {
			aliases = append(aliases, model.Alias)
		}
		byAlias[model.Alias] = append(byAlias[model.Alias], candidate)
	}
	fallbacks := make(map[string][]string)
	for _, entry := range configuredFallbacks {
		for alias, targets := range entry {
			_, exists := fallbacks[alias]
			if len(byAlias[alias]) == 0 || exists {
				return nil, fmt.Errorf("unknown or repeated fallback alias %q", alias)
			}
			fallbacks[alias] = targets
		}
	}
	routes := make([]runtime.Route, 0, len(aliases))
	for _, alias := range aliases {
		candidates := append([]runtime.Candidate(nil), byAlias[alias]...)
		seen := map[string]bool{alias: true}
		for _, target := range fallbacks[alias] {
			if seen[target] || len(byAlias[target]) == 0 {
				return nil, fmt.Errorf("model %q has an unknown or repeated fallback %q", alias, target)
			}
			seen[target] = true
			candidates = append(candidates, byAlias[target]...)
		}
		if len(candidates) > 16 {
			return nil, fmt.Errorf("model %q exceeds the 16-candidate limit", alias)
		}
		for _, candidate := range candidates {
			if !runtime.CompatibleEndpoints(candidate.Endpoint, candidates[0].Endpoint) {
				return nil, fmt.Errorf("model %q mixes incompatible endpoints", alias)
			}
		}
		routes = append(routes, runtime.Route{Alias: alias, Version: "1", Candidates: candidates})
	}
	return routes, nil
}

func (entry deployment) candidate() (runtime.Candidate, error) {
	provider, model, found := strings.Cut(entry.Params.Model, "/")
	if entry.Alias == "" || model == "" || !found || (!supportsOpenAITransport(provider) && provider != "anthropic" && provider != "gemini" && provider != "azure") {
		return runtime.Candidate{}, errors.New("model_name and a supported provider-prefixed model are required")
	}
	if provider == "openai" {
		model, _ = strings.CutPrefix(model, "chat_completions/")
	}
	if model == "" {
		return runtime.Candidate{}, errors.New("model ID is required")
	}
	base, err := resolveEnv(entry.Params.APIBase)
	if err != nil {
		return runtime.Candidate{}, fmt.Errorf("api_base: %w", err)
	}
	if base == "" && provider == "anthropic" {
		base = "https://api.anthropic.com"
	}
	if base == "" && provider == "gemini" {
		base = "https://generativelanguage.googleapis.com"
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return runtime.Candidate{}, errors.New("api_base must be an HTTP or HTTPS URL without credentials, query or fragment")
	}
	key, err := resolveEnv(entry.Params.APIKey)
	if err != nil {
		return runtime.Candidate{}, fmt.Errorf("api_key: %w", err)
	}
	version, err := resolveEnv(entry.Params.APIVersion)
	if err != nil {
		return runtime.Candidate{}, fmt.Errorf("api_version: %w", err)
	}
	if provider == "azure" && version == "" && !strings.HasSuffix(strings.TrimRight(base, "/"), "/openai/v1") {
		return runtime.Candidate{}, errors.New("Azure deployment API requires api_version")
	}
	endpoint := "chat-completions"
	if provider == "anthropic" {
		endpoint = "anthropic-messages"
	}
	if provider == "gemini" {
		endpoint = "gemini-generate"
	}
	if len(entry.Info.Endpoints) > 1 {
		return runtime.Candidate{}, errors.New("each deployment accepts one supported_endpoints entry")
	}
	if len(entry.Info.Endpoints) == 1 {
		endpoint = runtime.EndpointFromPath(entry.Info.Endpoints[0])
		if endpoint == "" || provider == "gemini" || (endpoint == "anthropic-messages") != (provider == "anthropic") {
			return runtime.Candidate{}, errors.New("unsupported model endpoint")
		}
	}
	return runtime.Candidate{SourceID: provider, Provider: provider, ModelID: model,
		APIBase: base, APIKey: key, Endpoint: endpoint, APIVersion: version, DeploymentID: entry.Info.ID}, nil
}

func resolveEnv(value string) (string, error) {
	name, referenced := strings.CutPrefix(value, "os.environ/")
	if !referenced {
		return value, nil
	}
	resolved := os.Getenv(name)
	if resolved == "" {
		return "", fmt.Errorf("environment variable %q is missing", name)
	}
	return resolved, nil
}
