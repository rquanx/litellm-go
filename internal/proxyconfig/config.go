package proxyconfig

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/rquanx/minato-runtime/internal/runtime"
	"go.yaml.in/yaml/v3"
)

type configFile struct {
	Models  []deployment `yaml:"model_list"`
	SearchTools []searchToolConfig `yaml:"search_tools"`
	MCPServers map[string]mcpServerConfig `yaml:"mcp_servers"`
	Credentials []providerCredential `yaml:"credential_list"`
	Callbacks callbackSettings `yaml:"litellm_settings"`
	General struct {
		MasterKey string `yaml:"master_key"`
		DatabaseURL string `yaml:"database_url"`
		StoreModels bool `yaml:"store_model_in_db"`
		DisableSpendLogs bool `yaml:"disable_spend_logs"`
		PublicReadinessDetails bool `yaml:"allow_public_health_readiness_details"`
		BackgroundHealthChecks bool `yaml:"background_health_checks"`
		HealthCheckInterval int `yaml:"health_check_interval"`
		HealthCheckConcurrency int `yaml:"health_check_concurrency"`
		SkipDisabledHealthChecks bool `yaml:"health_check_skip_disabled_background_models"`
		EnableHealthCheckRouting bool `yaml:"enable_health_check_routing"`
		HealthCheckStaleness int `yaml:"health_check_staleness_threshold"`
		IgnoreTransientHealthErrors bool `yaml:"health_check_ignore_transient_errors"`
		PassThrough []passThroughConfig `yaml:"pass_through_endpoints"`
		SearchTools []searchToolConfig `yaml:"search_tools"`
	} `yaml:"general_settings"`
	Router struct {
		Fallbacks []map[string][]string `yaml:"fallbacks"`
		DefaultFallbacks []string       `yaml:"default_fallbacks"`
		ContextFallbacks []map[string][]string `yaml:"context_window_fallbacks"`
		PolicyFallbacks []map[string][]string `yaml:"content_policy_fallbacks"`
		Strategy string                 `yaml:"routing_strategy"`
		ModelGroupAliases map[string]modelGroupAlias `yaml:"model_group_alias"`
		StrategyArgs struct {
			Buffer float64 `yaml:"lowest_latency_buffer"`
			Samples int `yaml:"max_latency_list_size"`
		} `yaml:"routing_strategy_args"`
		Timeout   int                  `yaml:"timeout"`
		Retries   int                  `yaml:"num_retries"`
		AllowedFails *int `yaml:"allowed_fails"`
		CooldownTime *float64 `yaml:"cooldown_time"`
		DisableCooldowns bool `yaml:"disable_cooldowns"`
		RetryPolicy runtime.RetryPolicy `yaml:"retry_policy"`
		ModelRetryPolicies map[string]runtime.RetryPolicy `yaml:"model_group_retry_policy"`
	} `yaml:"router_settings"`
}

type deployment struct {
	Alias  string `yaml:"model_name" json:"model_name"`
	Params struct {
		Model   string `yaml:"model" json:"model"`
		APIBase string `yaml:"api_base" json:"api_base,omitempty"`
		APIKey  string `yaml:"api_key" json:"api_key,omitempty"`
		UseInPassThrough bool `yaml:"use_in_pass_through" json:"use_in_pass_through,omitempty"`
		CredentialName string `yaml:"litellm_credential_name" json:"litellm_credential_name,omitempty"`
		APIVersion string `yaml:"api_version" json:"api_version,omitempty"`
		AzureADToken string `yaml:"azure_ad_token" json:"azure_ad_token,omitempty"`
		Weight *float64 `yaml:"weight" json:"weight,omitempty"`
		InputPrice *float64 `yaml:"input_cost_per_token" json:"input_cost_per_token,omitempty"`
		OutputPrice *float64 `yaml:"output_cost_per_token" json:"output_cost_per_token,omitempty"`
		CacheReadPrice *float64 `yaml:"cache_read_input_token_cost" json:"cache_read_input_token_cost,omitempty"`
		CacheCreationPrice *float64 `yaml:"cache_creation_input_token_cost" json:"cache_creation_input_token_cost,omitempty"`
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
		Mode      string   `yaml:"mode" json:"mode,omitempty"`
		DisableBackgroundHealthCheck bool `yaml:"disable_background_health_check" json:"disable_background_health_check,omitempty"`
	} `yaml:"model_info" json:"model_info"`
}

type Config struct {
	Routes    []runtime.Route
	SearchTools []runtime.SearchTool
	MCPServers []runtime.MCPServer
	staticMCPServers map[string]mcpServerConfig
	SearchFallbacks map[string][]string
	staticSearchTools []searchToolConfig
	MasterKey string
	Timeout   time.Duration
	NumRetries int
	RoutingStrategy string
	LatencyBuffer float64
	LatencySamples int
	AllowedFails int
	Cooldown time.Duration
	RetryPolicy runtime.RetryPolicy
	ModelRetryPolicies map[string]runtime.RetryPolicy
	DatabaseURL string
	DisableSpendLogs bool
	PublicReadinessDetails bool
	BackgroundHealthInterval time.Duration
	HealthCheckConcurrency int
	SkipDisabledHealthChecks bool
	HealthRoutingStaleness time.Duration
	IgnoreTransientHealthErrors bool
	PassThrough []runtime.PassThroughRoute
	staticPassThrough []passThroughConfig
	models    []deployment
	fallbacks []map[string][]string
	defaultFallbacks []string
	contextFallbacks []map[string][]string
	policyFallbacks []map[string][]string
	modelGroupAliases map[string]modelGroupAlias
	credentials map[string]providerCredential
	Callback *GenericCallback
	LogMessages bool
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
	if len(file.SearchTools) == 0 {
		file.SearchTools = file.General.SearchTools
	}
	if len(file.Models)+len(file.SearchTools)+len(file.General.PassThrough)+len(file.MCPServers) == 0 || file.Router.Timeout < 0 || file.Router.Timeout > 86400 || file.Router.Retries < 0 || file.Router.Retries > 8 {
		return Config{}, errors.New("configure a model, search tool, pass-through endpoint or MCP server and use valid router settings")
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
	allowedFails := 0
	if file.Router.AllowedFails != nil {
		allowedFails = *file.Router.AllowedFails
	}
	cooldownSeconds := 60.0
	if file.Router.CooldownTime != nil {
		cooldownSeconds = *file.Router.CooldownTime
	}
	if allowedFails < 0 || cooldownSeconds < 0 || cooldownSeconds > 86400 || math.IsNaN(cooldownSeconds) {
		return Config{}, errors.New("invalid cooldown settings")
	}
	if file.Router.DisableCooldowns || (file.Router.AllowedFails == nil && file.Router.CooldownTime == nil) {
		cooldownSeconds = 0
	}
	if !file.Router.RetryPolicy.Valid() {
		return Config{}, errors.New("invalid retry_policy")
	}
	for _, policy := range file.Router.ModelRetryPolicies {
		if !policy.Valid() {
			return Config{}, errors.New("invalid model_group_retry_policy")
		}
	}
	config := Config{Timeout: time.Duration(file.Router.Timeout) * time.Second, NumRetries: file.Router.Retries,
		RoutingStrategy: strategy, models: file.Models, fallbacks: file.Router.Fallbacks, defaultFallbacks: file.Router.DefaultFallbacks,
		contextFallbacks: file.Router.ContextFallbacks, policyFallbacks: file.Router.PolicyFallbacks,
		LatencyBuffer: args.Buffer, LatencySamples: args.Samples, modelGroupAliases: file.Router.ModelGroupAliases,
		DisableSpendLogs: file.General.DisableSpendLogs, PublicReadinessDetails: file.General.PublicReadinessDetails, AllowedFails: allowedFails,
		Cooldown: time.Duration(cooldownSeconds * float64(time.Second)), RetryPolicy: file.Router.RetryPolicy,
		ModelRetryPolicies: file.Router.ModelRetryPolicies}
	config.credentials = make(map[string]providerCredential, len(file.Credentials))
	if file.General.HealthCheckInterval < 0 || file.General.HealthCheckInterval > 86400 || file.General.HealthCheckConcurrency < 0 ||
		file.General.HealthCheckConcurrency > 1024 || file.General.HealthCheckStaleness < 0 || file.General.HealthCheckStaleness > 86400 {
		return Config{}, errors.New("invalid health check interval or concurrency")
	}
	config.HealthCheckConcurrency = file.General.HealthCheckConcurrency
	config.SkipDisabledHealthChecks = file.General.SkipDisabledHealthChecks
	config.IgnoreTransientHealthErrors = file.General.IgnoreTransientHealthErrors
	if file.General.BackgroundHealthChecks {
		interval := file.General.HealthCheckInterval
		if interval == 0 {
			interval = 300
		}
		config.BackgroundHealthInterval = time.Duration(interval) * time.Second
	}
	if file.General.EnableHealthCheckRouting {
		config.HealthRoutingStaleness = time.Duration(file.General.HealthCheckStaleness) * time.Second
		if config.HealthRoutingStaleness == 0 {
			config.HealthRoutingStaleness = 10 * time.Minute
		}
	}
	for _, credential := range file.Credentials {
		if err := credential.validate(); err != nil {
			return Config{}, err
		}
		if _, exists := config.credentials[credential.Name]; exists {
			return Config{}, errors.New("duplicate credential name")
		}
		config.credentials[credential.Name] = credential
	}
	var err error
	config.MCPServers, err = prepareMCPServers(file.MCPServers)
	if err != nil {
		return Config{}, err
	}
	config.staticMCPServers = file.MCPServers
	config.PassThrough, err = preparePassThrough(file.General.PassThrough)
	if err != nil {
		return Config{}, err
	}
	config.staticPassThrough = file.General.PassThrough
	config.SearchTools, err = prepareSearchTools(file.SearchTools)
	if err != nil {
		return Config{}, err
	}
	config.staticSearchTools = file.SearchTools
	config.Callback, config.LogMessages, err = prepareCallback(file.Callbacks)
	if err != nil {
		return Config{}, err
	}
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
	config.SearchFallbacks = config.searchFallbacks()
	return config, nil
}

func (config Config) buildRoutes(models []deployment) ([]runtime.Route, error) {
	byAlias := make(map[string][]runtime.Candidate)
	var aliases []string
	for _, model := range models {
		effective, err := model.withCredential(config.credentials)
		if err != nil {
			return nil, fmt.Errorf("model %q: %w", model.Alias, err)
		}
		candidates, err := effective.candidates()
		if err != nil {
			return nil, fmt.Errorf("model %q: %w", model.Alias, err)
		}
		if _, exists := byAlias[model.Alias]; !exists {
			aliases = append(aliases, model.Alias)
		}
		byAlias[model.Alias] = append(byAlias[model.Alias], candidates...)
	}
	for alias, mapping := range config.modelGroupAliases {
		if alias == "" || alias == "*" || byAlias[alias] != nil {
			return nil, fmt.Errorf("invalid or repeated model group alias %q", alias)
		}
		if byAlias[mapping.Model] == nil || config.modelGroupAliases[mapping.Model].Model != "" {
			return nil, fmt.Errorf("model group alias %q must target a configured model", alias)
		}
	}
	for alias, mapping := range config.modelGroupAliases {
		candidates := slices.Clone(byAlias[mapping.Model])
		for index := range candidates {
			candidates[index].RoutingGroup = alias
		}
		byAlias[alias] = candidates
		aliases = append(aliases, alias)
	}
	searchNames := make(map[string]bool, len(config.SearchTools))
	for _, tool := range config.SearchTools {
		searchNames[tool.Name] = true
	}
	fallbacks := make(map[string][]string)
	for _, entry := range config.fallbacks {
		for alias, targets := range entry {
			_, exists := fallbacks[alias]
			if (alias != "*" && len(byAlias[alias]) == 0 && !searchNames[alias]) || exists {
				return nil, fmt.Errorf("unknown or repeated fallback alias %q", alias)
			}
			fallbacks[alias] = targets
			for _, target := range targets {
				if byAlias[target] == nil && !searchNames[target] {
					return nil, fmt.Errorf("unknown fallback target %q", target)
				}
				if alias != "*" && byAlias[alias] == nil && (!searchNames[target] || target == alias) {
					return nil, fmt.Errorf("search tool %q has an incompatible fallback %q", alias, target)
				}
			}
		}
	}
	for _, alias := range config.defaultFallbacks {
		if len(byAlias[alias]) == 0 && !searchNames[alias] {
			return nil, fmt.Errorf("unknown default fallback %q", alias)
		}
	}
	routes := make([]runtime.Route, 0, len(aliases))
	for _, alias := range aliases {
		candidates := append([]runtime.Candidate(nil), byAlias[alias]...)
		seen := map[string]bool{alias: true}
		targets, explicit := fallbacks[alias]
		if !explicit {
			targets, explicit = fallbacks[config.modelGroupAliases[alias].Model]
		}
		if !explicit {
			targets = fallbacks["*"]
		}
		for _, target := range targets {
			if explicit && searchNames[alias] && searchNames[target] && byAlias[target] == nil {
				continue
			}
			if !explicit && (target == alias || target == config.modelGroupAliases[alias].Model ||
				!hasCompatibleDeployment(byAlias[alias], byAlias[target])) {
				continue
			}
			if seen[target] || len(byAlias[target]) == 0 {
				return nil, fmt.Errorf("model %q has an unknown or repeated fallback %q", alias, target)
			}
			if !hasCompatibleDeployment(byAlias[alias], byAlias[target]) {
				return nil, fmt.Errorf("model %q has an incompatible fallback %q", alias, target)
			}
			seen[target] = true
			candidates = append(candidates, byAlias[target]...)
		}
		for _, target := range config.defaultFallbacks {
			if !seen[target] && hasCompatibleDeployment(byAlias[alias], byAlias[target]) {
				candidates = append(candidates, byAlias[target]...)
				seen[target] = true
			}
		}
		if len(candidates) > 16 {
			return nil, fmt.Errorf("model %q exceeds the 16-candidate limit", alias)
		}
		routes = append(routes, runtime.Route{Alias: alias, Version: "1", Candidates: candidates, Hidden: config.modelGroupAliases[alias].Hidden})
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
				candidates, exists = overrides[config.modelGroupAliases[routes[index].Alias].Model]
			}
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

func hasCompatibleDeployment(primary, fallback []runtime.Candidate) bool {
	for _, first := range primary {
		for _, other := range fallback {
			if runtime.CompatibleEndpoints(first.Endpoint, other.Endpoint) {
				return true
			}
		}
	}
	return false
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
				if !hasCompatibleDeployment(primary, deployments[target]) {
					return nil, fmt.Errorf("model %q has an incompatible fallback %q", model, target)
				}
				candidates = append(candidates, deployments[target]...)
			}
			if len(candidates) > 16 {
				return nil, fmt.Errorf("model %q exceeds the 16-candidate limit", model)
			}
			fallbacks[model] = candidates
		}
	}
	return fallbacks, nil
}

func (entry deployment) candidates() ([]runtime.Candidate, error) {
	paths := entry.Info.Endpoints
	if len(paths) <= 1 {
		candidate, err := entry.candidate()
		if err != nil {
			return nil, err
		}
		return []runtime.Candidate{candidate}, nil
	}
	candidates := make([]runtime.Candidate, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		entry.Info.Endpoints = []string{path}
		candidate, err := entry.candidate()
		if err != nil {
			return nil, err
		}
		if !seen[candidate.Endpoint] {
			seen[candidate.Endpoint] = true
			candidates = append(candidates, candidate)
		}
	}
	return candidates, nil
}

func (entry deployment) candidate() (runtime.Candidate, error) {
	provider, model, found := strings.Cut(entry.Params.Model, "/")
	infinityRerank := provider == "infinity" && entry.rerankConfigured()
	if entry.Alias == "" || model == "" || !found || (!supportsOpenAITransport(provider) && provider != "anthropic" &&
		provider != "gemini" && provider != "vertex_ai" && provider != "azure" && provider != "bedrock" && !rerankProvider(provider) && !infinityRerank) {
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
	if base == "" && entry.rerankConfigured() {
		switch provider {
		case "fireworks_ai":
			base = "https://api.fireworks.ai"
		case "infinity":
			base = os.Getenv("INFINITY_API_BASE")
		case "deepinfra":
			base = os.Getenv("DEEPINFRA_API_BASE")
		case "together_ai":
			base = "https://api.together.xyz"
		case "nvidia_nim":
			base = os.Getenv("NVIDIA_NIM_API_BASE")
			if base == "" {
				base = "https://ai.api.nvidia.com"
			}
		}
	}
	var bedrock *runtime.BedrockConfig
	if provider == "bedrock" {
		bedrock, err = entry.bedrockConfig()
		if err != nil {
			return runtime.Candidate{}, err
		}
		if base == "" {
			service := "bedrock-runtime"
			if entry.bedrockRerank() {
				service = "bedrock-agent-runtime"
			}
			base = "https://" + service + "." + bedrock.Region + ".amazonaws.com"
		}
	}
	if base == "" && rerankProvider(provider) {
		if provider == "voyage" {
			base = "https://api.voyageai.com"
		} else if provider == "jina_ai" {
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
	if key == "" && entry.rerankConfigured() {
		switch provider {
		case "fireworks_ai":
			key = os.Getenv("FIREWORKS_API_KEY")
			if key == "" {
				key = os.Getenv("FIREWORKS_AI_API_KEY")
			}
		case "hosted_vllm":
			key = os.Getenv("HOSTED_VLLM_API_KEY")
			if key == "" {
				key = "fake-api-key"
			}
		case "infinity":
			key = os.Getenv("INFINITY_API_KEY")
		case "deepinfra":
			key = os.Getenv("DEEPINFRA_API_KEY")
		case "together_ai":
			key = os.Getenv("TOGETHERAI_API_KEY")
		case "nvidia_nim":
			key = os.Getenv("NVIDIA_NIM_API_KEY")
		}
	}
	if key == "" && rerankProvider(provider) {
		if provider == "voyage" {
			key = os.Getenv("VOYAGE_API_KEY")
		} else if provider == "jina_ai" {
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
	var azureADToken string
	if provider == "azure" {
		azureADToken, err = resolveEnv(entry.Params.AzureADToken)
		if err != nil {
			return runtime.Candidate{}, fmt.Errorf("azure_ad_token: %w", err)
		}
		if azureADToken == "" && key == "" && parsed.Scheme == "https" &&
			strings.HasSuffix(parsed.Hostname(), ".openai.azure.com") {
			azureADToken = os.Getenv("AZURE_AD_TOKEN")
		}
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
		if provider == "vertex_ai" && (entry.Info.Mode == "rerank" || strings.HasPrefix(model, "semantic-ranker-")) {
			endpoint = "rerank"
		}
	}
	if provider == "bedrock" {
		endpoint = "bedrock-converse"
		if strings.Contains(model, "amazon.titan-embed-text") {
			endpoint = "bedrock-embeddings"
		}
		if entry.bedrockRerank() {
			endpoint = "rerank"
		}
	}
	if rerankProvider(provider) {
		endpoint = "rerank"
	}
	if (provider == "cohere" || provider == "cohere_chat") && (entry.Info.Mode == "embedding" || strings.HasPrefix(model, "embed-")) {
		endpoint = "cohere-embeddings"
	}
	if provider == "voyage" && entry.Info.Mode == "embedding" {
		endpoint = "embeddings"
	}
	if supportsOpenAITransport(provider) || provider == "azure" || infinityRerank {
		if inferred := openAIModelEndpoint(entry.Info.Mode, model); inferred != "" {
			endpoint = inferred
		}
	}
	if len(entry.Info.Endpoints) == 1 {
		configured := runtime.EndpointFromPath(entry.Info.Endpoints[0])
		if endpoint == "bedrock-converse" && (configured == "chat-completions" || configured == "responses") {
			configured = endpoint
		}
		if configured == "embeddings" && (endpoint == "bedrock-embeddings" || endpoint == "google-embeddings") {
			configured = endpoint
		}
		if configured == "embeddings" && (provider == "cohere" || provider == "cohere_chat") {
			configured = "cohere-embeddings"
		}
		if configured == "" || ((provider == "gemini" || provider == "vertex_ai") && configured != "google-embeddings" &&
			!(provider == "vertex_ai" && configured == "rerank")) ||
			(configured == "anthropic-messages") != (provider == "anthropic") ||
			(provider == "bedrock" && configured != endpoint) {
			return runtime.Candidate{}, errors.New("unsupported model endpoint")
		}
		endpoint = configured
	}
	price, usagePrice, err := tokenPrice(entry.Params.Model, entry.Params.InputPrice, entry.Params.OutputPrice)
	if err != nil {
		return runtime.Candidate{}, err
	}
	cachePrices, err := cacheTokenPrices(entry.Params.Model, entry.Params.CacheReadPrice, entry.Params.CacheCreationPrice)
	if err != nil {
		return runtime.Candidate{}, err
	}
	return runtime.Candidate{SourceID: provider, Provider: provider, ModelID: model,
		APIBase: base, APIKey: key, Endpoint: endpoint, APIVersion: version, DeploymentID: entry.Info.ID,
		Headers: headers, ParamMappings: settings.ParamMappings, RelativeAPIBase: hasSettings, TextOnly: settings.Special.TextOnly,
		RoutingGroup: entry.Alias, Weight: entry.Params.Weight, RPM: entry.Params.RPM, TPM: entry.Params.TPM, Bedrock: bedrock,
		DisableBackgroundHealthCheck: entry.Info.DisableBackgroundHealthCheck,
		UseInPassThrough: entry.Params.UseInPassThrough,
		TokenPrice: price, UsagePrice: usagePrice, CacheReadPrice: cachePrices[0], CacheCreationPrice: cachePrices[1],
		Vertex: vertex, AzureADToken: azureADToken}, nil
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
	return name == "cohere" || name == "cohere_chat" || name == "jina_ai" || name == "voyage"
}

func (entry deployment) rerankConfigured() bool {
	return entry.Info.Mode == "rerank" || slices.Contains(entry.Info.Endpoints, "/v1/rerank")
}

func (entry deployment) bedrockRerank() bool {
	return entry.rerankConfigured() || strings.Contains(entry.Params.Model, ".rerank-")
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
