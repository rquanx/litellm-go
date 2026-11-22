package proxyconfig

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
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
		ContextFallbacks []map[string][]string `yaml:"context_window_fallbacks"`
		PolicyFallbacks []map[string][]string `yaml:"content_policy_fallbacks"`
		Strategy string                 `yaml:"routing_strategy"`
		StrategyArgs struct {
			Buffer float64 `yaml:"lowest_latency_buffer"`
			Samples int `yaml:"max_latency_list_size"`
		} `yaml:"routing_strategy_args"`
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
		InputPrice *float64 `yaml:"input_cost_per_token" json:"input_cost_per_token,omitempty"`
		OutputPrice *float64 `yaml:"output_cost_per_token" json:"output_cost_per_token,omitempty"`
		RPM *int `yaml:"rpm" json:"rpm,omitempty"`
		TPM *int `yaml:"tpm" json:"tpm,omitempty"`
		AWSRegion string `yaml:"aws_region_name" json:"aws_region_name,omitempty"`
		AWSAccessKey string `yaml:"aws_access_key_id" json:"aws_access_key_id,omitempty"`
		AWSSecretKey string `yaml:"aws_secret_access_key" json:"aws_secret_access_key,omitempty"`
		AWSSessionToken string `yaml:"aws_session_token" json:"aws_session_token,omitempty"`
		AWSProfile string `yaml:"aws_profile_name" json:"aws_profile_name,omitempty"`
		VertexProject string `yaml:"vertex_project" json:"vertex_project,omitempty"`
		VertexLocation string `yaml:"vertex_location" json:"vertex_location,omitempty"`
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
	LatencyBuffer float64
	LatencySamples int
	DatabaseURL string
	models    []deployment
	fallbacks []map[string][]string
	defaultFallbacks []string
	contextFallbacks []map[string][]string
	policyFallbacks []map[string][]string
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
	switch strategy {
	case "simple-shuffle", "least-busy", "cost-based-routing", "latency-based-routing", "usage-based-routing", "usage-based-routing-v2":
	default:
		return Config{}, errors.New("unsupported routing_strategy")
	}
	args := file.Router.StrategyArgs
	if args.Buffer < 0 || math.IsNaN(args.Buffer) || math.IsInf(args.Buffer, 0) || args.Samples < 0 || args.Samples > 1000 {
		return Config{}, errors.New("invalid latency routing settings")
	}
	config := Config{Timeout: time.Duration(file.Router.Timeout) * time.Second, NumRetries: file.Router.Retries,
		RoutingStrategy: strategy, models: file.Models, fallbacks: file.Router.Fallbacks, defaultFallbacks: file.Router.DefaultFallbacks,
		contextFallbacks: file.Router.ContextFallbacks, policyFallbacks: file.Router.PolicyFallbacks,
		LatencyBuffer: args.Buffer, LatencySamples: args.Samples}
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
	config.Routes, err = config.buildRoutes(config.models)
	if err != nil {
		return Config{}, err
	}
	return config, nil
}

func (config Config) buildRoutes(models []deployment) ([]runtime.Route, error) {
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
	for _, entry := range config.fallbacks {
		for alias, targets := range entry {
			_, exists := fallbacks[alias]
			if len(byAlias[alias]) == 0 || exists {
				return nil, fmt.Errorf("unknown or repeated fallback alias %q", alias)
			}
			fallbacks[alias] = targets
		}
	}
	for _, alias := range config.defaultFallbacks {
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
		for _, target := range config.defaultFallbacks {
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
	for _, policy := range []struct {
		name string
		entries []map[string][]string
	}{
		{"context_window_fallbacks", config.contextFallbacks},
		{"content_policy_fallbacks", config.policyFallbacks},
	} {
		if policy.entries == nil {
			continue
		}
		overrides, err := compileSpecialFallbacks(policy.entries, byAlias)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", policy.name, err)
		}
		for index := range routes {
			candidates, exists := overrides[routes[index].Alias]
			if !exists {
				candidates = []runtime.Candidate{}
			}
			if policy.name == "context_window_fallbacks" {
				routes[index].ContextFallback = candidates
			} else {
				routes[index].PolicyFallback = candidates
			}
		}
	}
	return routes, nil
}

func compileSpecialFallbacks(entries []map[string][]string, deployments map[string][]runtime.Candidate) (map[string][]runtime.Candidate, error) {
	fallbacks := make(map[string][]runtime.Candidate)
	for _, entry := range entries {
		for model, targets := range entry {
			primary := deployments[model]
			if _, exists := fallbacks[model]; len(primary) == 0 || exists {
				return nil, fmt.Errorf("unknown or repeated model %q", model)
			}
			candidates := []runtime.Candidate{}
			seen := map[string]bool{model: true}
			for _, target := range targets {
				if seen[target] || len(deployments[target]) == 0 {
					return nil, fmt.Errorf("model %q has an unknown or repeated fallback %q", model, target)
				}
				seen[target] = true
				for _, candidate := range deployments[target] {
					if !runtime.CompatibleEndpoints(primary[0].Endpoint, candidate.Endpoint) {
						return nil, fmt.Errorf("model %q has an incompatible fallback %q", model, target)
					}
					candidates = append(candidates, candidate)
				}
			}
			if len(candidates) > 16 {
				return nil, fmt.Errorf("model %q exceeds the 16-candidate limit", model)
			}
			fallbacks[model] = candidates
		}
	}
	return fallbacks, nil
}

func (entry deployment) candidate() (runtime.Candidate, error) {
	provider, model, found := strings.Cut(entry.Params.Model, "/")
	if entry.Alias == "" || model == "" || !found || (!supportsOpenAITransport(provider) && provider != "anthropic" &&
		provider != "gemini" && provider != "vertex_ai" && provider != "azure" && provider != "bedrock" && !rerankProvider(provider)) {
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
	var vertex *runtime.VertexConfig
	if provider == "vertex_ai" {
		vertex, base, err = entry.vertexConfig(base)
		if err != nil {
			return runtime.Candidate{}, err
		}
	}
	if base == "" && hasSettings {
		base = os.Getenv(settings.APIBaseEnv)
		if base == "" {
			base = settings.BaseURL
		}
	}
	if base == "" && provider == "openai" {
		base = "https://api.openai.com"
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
	if key == "" && provider == "openai" && parsed.Scheme == "https" && parsed.Host == "api.openai.com" {
		key = os.Getenv("OPENAI_API_KEY")
	}
	if key == "" && provider == "bedrock" {
		key = os.Getenv("AWS_BEARER_TOKEN_BEDROCK")
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
	if provider == "gemini" || provider == "vertex_ai" {
		endpoint = "gemini-generate"
		if strings.Contains(model, "embedding") {
			endpoint = "google-embeddings"
		}
	}
	if provider == "bedrock" {
		endpoint = "bedrock-converse"
		if strings.Contains(model, "amazon.titan-embed-text") {
			endpoint = "bedrock-embeddings"
		}
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
		if endpoint == "bedrock-converse" && (configured == "chat-completions" || configured == "responses") {
			configured = endpoint
		}
		if configured == "embeddings" && (endpoint == "bedrock-embeddings" || endpoint == "google-embeddings") {
			configured = endpoint
		}
		if configured == "" || ((provider == "gemini" || provider == "vertex_ai") && configured != "google-embeddings") ||
			(configured == "anthropic-messages") != (provider == "anthropic") ||
			(provider == "bedrock" && configured != endpoint) {
			return runtime.Candidate{}, errors.New("unsupported model endpoint")
		}
		endpoint = configured
	}
	price, err := tokenPrice(entry.Params.Model, entry.Params.InputPrice, entry.Params.OutputPrice)
	if err != nil {
		return runtime.Candidate{}, err
	}
	return runtime.Candidate{SourceID: provider, Provider: provider, ModelID: model,
		APIBase: base, APIKey: key, Endpoint: endpoint, APIVersion: version, DeploymentID: entry.Info.ID,
		Headers: headers, ParamMappings: settings.ParamMappings, RelativeAPIBase: hasSettings, TextOnly: settings.Special.TextOnly,
		RoutingGroup: entry.Alias, Weight: entry.Params.Weight, RPM: entry.Params.RPM, TPM: entry.Params.TPM, Bedrock: bedrock,
		TokenPrice: price, Vertex: vertex}, nil
}

func (entry deployment) bedrockConfig() (*runtime.BedrockConfig, error) {
	config := &runtime.BedrockConfig{}
	for _, field := range []struct {
		value, variable string
		target *string
	}{
		{entry.Params.AWSRegion, "AWS_REGION", &config.Region},
		{entry.Params.AWSAccessKey, "", &config.AccessKey},
		{entry.Params.AWSSecretKey, "", &config.SecretKey},
		{entry.Params.AWSSessionToken, "", &config.SessionToken},
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
	if (config.AccessKey == "") != (config.SecretKey == "") {
		return nil, errors.New("Bedrock requires both explicit AWS access and secret keys")
	}
	profile, err := resolveEnv(entry.Params.AWSProfile)
	if err != nil {
		return nil, err
	}
	if config.Region == "" || (config.AccessKey == "" && entry.Params.APIKey == "" && os.Getenv("AWS_BEARER_TOKEN_BEDROCK") == "") {
		options := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(config.Region)}
		if profile != "" {
			options = append(options, awsconfig.WithSharedConfigProfile(profile))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		awsConfig, err := awsconfig.LoadDefaultConfig(ctx, options...)
		if err != nil {
			return nil, errors.New("could not load the AWS configuration for Bedrock")
		}
		config.Region = awsConfig.Region
		config.Credentials = awsConfig.Credentials
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
