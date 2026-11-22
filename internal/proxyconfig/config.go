package proxyconfig

import (
	"errors"
	"fmt"
	"io"
	"math"
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
		DatabaseURL string `yaml:"database_url"`
		StoreModels bool `yaml:"store_model_in_db"`
	} `yaml:"general_settings"`
	Router struct {
		Fallbacks []map[string][]string `yaml:"fallbacks"`
		DefaultFallbacks []string       `yaml:"default_fallbacks"`
		Strategy string                 `yaml:"routing_strategy"`
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
		Weight *float64 `yaml:"weight" json:"weight,omitempty"`
		RPM *int `yaml:"rpm" json:"rpm,omitempty"`
		TPM *int `yaml:"tpm" json:"tpm,omitempty"`
		AWSRegion string `yaml:"aws_region_name" json:"aws_region_name,omitempty"`
		AWSAccessKey string `yaml:"aws_access_key_id" json:"aws_access_key_id,omitempty"`
		AWSSecretKey string `yaml:"aws_secret_access_key" json:"aws_secret_access_key,omitempty"`
		AWSSessionToken string `yaml:"aws_session_token" json:"aws_session_token,omitempty"`
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
	RoutingStrategy string
	DatabaseURL string
	models    []deployment
	fallbacks []map[string][]string
	defaultFallbacks []string
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
	strategy := file.Router.Strategy
	if strategy == "" {
		strategy = "simple-shuffle"
	}
	if strategy != "simple-shuffle" && strategy != "least-busy" {
		return Config{}, errors.New("unsupported routing_strategy")
	}
	config := Config{Timeout: time.Duration(file.Router.Timeout) * time.Second, NumRetries: file.Router.Retries,
		RoutingStrategy: strategy, models: file.Models, fallbacks: file.Router.Fallbacks, defaultFallbacks: file.Router.DefaultFallbacks}
	var err error
	config.MasterKey, err = resolveEnv(file.General.MasterKey)
	if err != nil {
		return Config{}, fmt.Errorf("general_settings.master_key: %w", err)
	}
	if file.General.StoreModels || file.General.DatabaseURL != "" {
		config.DatabaseURL, err = resolveEnv(file.General.DatabaseURL)
		if err != nil {
			return Config{}, fmt.Errorf("general_settings.database_url: %w", err)
		}
		if config.DatabaseURL == "" {
			return Config{}, errors.New("general_settings.database_url is required for database state")
		}
	}
	for index := range config.models {
		model := &config.models[index]
		if model.Info.ID == "" {
			model.Info.ID = fmt.Sprintf("yaml:%s:%d", model.Alias, index)
		}
	}
	config.Routes, err = buildRoutes(config.models, config.fallbacks, config.defaultFallbacks)
	if err != nil {
		return Config{}, err
	}
	return config, nil
}

func buildRoutes(models []deployment, configuredFallbacks []map[string][]string, defaultFallbacks []string) ([]runtime.Route, error) {
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
	for _, alias := range defaultFallbacks {
		if len(byAlias[alias]) == 0 {
			return nil, fmt.Errorf("unknown default fallback %q", alias)
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
		for _, target := range defaultFallbacks {
			if !seen[target] && runtime.CompatibleEndpoints(candidates[0].Endpoint, byAlias[target][0].Endpoint) {
				candidates = append(candidates, byAlias[target]...)
				seen[target] = true
			}
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
	if entry.Alias == "" || model == "" || !found || (!supportsOpenAITransport(provider) && provider != "anthropic" &&
		provider != "gemini" && provider != "azure" && provider != "bedrock" && !rerankProvider(provider)) {
		return runtime.Candidate{}, errors.New("model_name and a supported provider-prefixed model are required")
	}
	if provider == "openai" {
		model, _ = strings.CutPrefix(model, "chat_completions/")
	}
	if provider == "bedrock" {
		model, _ = strings.CutPrefix(model, "converse/")
	}
	if model == "" {
		return runtime.Candidate{}, errors.New("model ID is required")
	}
	if (entry.Params.Weight != nil && (*entry.Params.Weight < 0 || math.IsNaN(*entry.Params.Weight) || math.IsInf(*entry.Params.Weight, 0))) ||
		(entry.Params.RPM != nil && *entry.Params.RPM < 0) || (entry.Params.TPM != nil && *entry.Params.TPM < 0) {
		return runtime.Candidate{}, errors.New("weight, rpm and tpm must be finite non-negative values")
	}
	settings, hasSettings := openAIProviderSettings[provider]
	base, err := resolveEnv(entry.Params.APIBase)
	if err != nil {
		return runtime.Candidate{}, fmt.Errorf("api_base: %w", err)
	}
	if base == "" && hasSettings {
		base = os.Getenv(settings.APIBaseEnv)
		if base == "" {
			base = settings.BaseURL
		}
	}
	if base == "" && provider == "anthropic" {
		base = "https://api.anthropic.com"
	}
	if base == "" && provider == "gemini" {
		base = "https://generativelanguage.googleapis.com"
	}
	var bedrock *runtime.BedrockConfig
	if provider == "bedrock" {
		bedrock, err = entry.bedrockConfig()
		if err != nil {
			return runtime.Candidate{}, err
		}
		if base == "" {
			base = "https://bedrock-runtime." + bedrock.Region + ".amazonaws.com"
		}
	}
	if base == "" && rerankProvider(provider) {
		if provider == "jina_ai" {
			base = "https://api.jina.ai"
		} else {
			base = "https://api.cohere.ai"
		}
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
	if key == "" && hasSettings {
		key = os.Getenv(settings.APIKeyEnv)
	}
	if key == "" && provider == "bedrock" {
		key = os.Getenv("AWS_BEARER_TOKEN_BEDROCK")
		if key == "" && (bedrock.AccessKey == "" || bedrock.SecretKey == "") {
			return runtime.Candidate{}, errors.New("Bedrock requires static AWS credentials or a bearer token")
		}
	}
	if key == "" && rerankProvider(provider) {
		if provider == "jina_ai" {
			key = os.Getenv("JINA_AI_API_KEY")
		} else {
			key = os.Getenv("COHERE_API_KEY")
		}
	}
	headers := make(map[string]string, len(settings.Headers))
	for name, value := range settings.Headers {
		headers[name] = strings.ReplaceAll(value, "{api_key}", key)
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
	if provider == "bedrock" {
		endpoint = "bedrock-converse"
	}
	if rerankProvider(provider) {
		endpoint = "rerank"
	}
	if len(entry.Info.Endpoints) > 1 {
		return runtime.Candidate{}, errors.New("each deployment accepts one supported_endpoints entry")
	}
	if rerankProvider(provider) && endpoint != "rerank" {
		return runtime.Candidate{}, errors.New("this provider requires a rerank endpoint")
	}
	if len(entry.Info.Endpoints) == 1 {
		configured := runtime.EndpointFromPath(entry.Info.Endpoints[0])
		if provider == "bedrock" && (configured == "chat-completions" || configured == "responses") {
			configured = endpoint
		}
		if configured == "" || provider == "gemini" || (configured == "anthropic-messages") != (provider == "anthropic") ||
			(configured == "bedrock-converse") != (provider == "bedrock") {
			return runtime.Candidate{}, errors.New("unsupported model endpoint")
		}
		endpoint = configured
	}
	return runtime.Candidate{SourceID: provider, Provider: provider, ModelID: model,
		APIBase: base, APIKey: key, Endpoint: endpoint, APIVersion: version, DeploymentID: entry.Info.ID,
		Headers: headers, ParamMappings: settings.ParamMappings, RelativeAPIBase: hasSettings, TextOnly: settings.Special.TextOnly,
		RoutingGroup: entry.Alias, Weight: entry.Params.Weight, RPM: entry.Params.RPM, TPM: entry.Params.TPM, Bedrock: bedrock}, nil
}

func (entry deployment) bedrockConfig() (*runtime.BedrockConfig, error) {
	config := &runtime.BedrockConfig{}
	for _, field := range []struct {
		value, variable string
		target *string
	}{
		{entry.Params.AWSRegion, "AWS_REGION", &config.Region},
		{entry.Params.AWSAccessKey, "AWS_ACCESS_KEY_ID", &config.AccessKey},
		{entry.Params.AWSSecretKey, "AWS_SECRET_ACCESS_KEY", &config.SecretKey},
		{entry.Params.AWSSessionToken, "AWS_SESSION_TOKEN", &config.SessionToken},
	} {
		value, err := resolveEnv(field.value)
		if err != nil {
			return nil, err
		}
		if value == "" {
			value = os.Getenv(field.variable)
		}
		*field.target = value
	}
	if config.Region == "" {
		config.Region = os.Getenv("AWS_DEFAULT_REGION")
	}
	if config.Region == "" {
		return nil, errors.New("Bedrock requires aws_region_name or AWS_REGION")
	}
	return config, nil
}

func rerankProvider(name string) bool {
	return name == "cohere" || name == "cohere_chat" || name == "jina_ai"
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
