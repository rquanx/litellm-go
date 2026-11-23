package proxyconfig

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/redis/go-redis/v9"
	"github.com/rquanx/minato-runtime/internal/runtime"
	"go.yaml.in/yaml/v3"
)

type configFile struct {
	Agents []agentConfig `yaml:"agent_list"`
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
		EnableTagFiltering bool `yaml:"enable_tag_filtering"`
		TagFilteringMatchAny *bool `yaml:"tag_filtering_match_any"`
		RedisURL string `yaml:"redis_url"`
		RedisHost string `yaml:"redis_host"`
		RedisPort string `yaml:"redis_port"`
		RedisPassword string `yaml:"redis_password"`
		RedisNamespace string `yaml:"redis_namespace"`
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
		MaxFallbacks *int `yaml:"max_fallbacks"`
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
		Region string `yaml:"region_name" json:"region_name,omitempty"`
		UseInPassThrough bool `yaml:"use_in_pass_through" json:"use_in_pass_through,omitempty"`
		CredentialName string `yaml:"litellm_credential_name" json:"litellm_credential_name,omitempty"`
		APIVersion string `yaml:"api_version" json:"api_version,omitempty"`
		AzureADToken string `yaml:"azure_ad_token" json:"azure_ad_token,omitempty"`
		AzureTenant string `yaml:"tenant_id" json:"tenant_id,omitempty"`
		AzureClient string `yaml:"client_id" json:"client_id,omitempty"`
		AzureUsername string `yaml:"azure_username" json:"azure_username,omitempty"`
		AzurePassword string `yaml:"azure_password" json:"azure_password,omitempty"`
		AzureClientSecret string `yaml:"client_secret" json:"client_secret,omitempty"`
		AzureScope string `yaml:"azure_scope" json:"azure_scope,omitempty"`
		AzureCredential string `yaml:"azure_credential" json:"azure_credential,omitempty"`
		AzureAuthority string `yaml:"azure_authority_host" json:"azure_authority_host,omitempty"`
		AzureTokenRefresh bool `yaml:"enable_azure_ad_token_refresh" json:"enable_azure_ad_token_refresh,omitempty"`
		Tags []string `yaml:"tags" json:"tags,omitempty"`
		TagRegex []string `yaml:"tag_regex" json:"tag_regex,omitempty"`
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
		AWSRole string `yaml:"aws_role_name" json:"aws_role_name,omitempty"`
		AWSRoleSession string `yaml:"aws_session_name" json:"aws_session_name,omitempty"`
		AWSWebIdentity string `yaml:"aws_web_identity_token" json:"aws_web_identity_token,omitempty"`
		AWSSTSEndpoint string `yaml:"aws_sts_endpoint" json:"aws_sts_endpoint,omitempty"`
		AWSExternalID string `yaml:"aws_external_id" json:"aws_external_id,omitempty"`
		AWSBedrockControlEndpoint string `yaml:"aws_bedrock_control_endpoint" json:"aws_bedrock_control_endpoint,omitempty"`
		AWSS3VectorsEndpoint string `yaml:"aws_s3vectors_endpoint" json:"aws_s3vectors_endpoint,omitempty"`
		AWSBedrockAgentEndpoint string `yaml:"aws_bedrock_agent_endpoint" json:"aws_bedrock_agent_endpoint,omitempty"`
		AWSBedrockAgentRuntimeEndpoint string `yaml:"aws_bedrock_agent_runtime_endpoint" json:"aws_bedrock_agent_runtime_endpoint,omitempty"`
		AWSBatchRoleARN string `yaml:"aws_batch_role_arn" json:"aws_batch_role_arn,omitempty"`
		S3OutputBucket string `yaml:"s3_output_bucket_name" json:"s3_output_bucket_name,omitempty"`
		S3Bucket string `yaml:"s3_bucket_name" json:"s3_bucket_name,omitempty"`
		S3Endpoint string `yaml:"s3_endpoint_url" json:"s3_endpoint_url,omitempty"`
		S3Region string `yaml:"s3_region_name" json:"s3_region_name,omitempty"`
		S3EncryptionKey string `yaml:"s3_encryption_key_id" json:"s3_encryption_key_id,omitempty"`
		VertexProject string `yaml:"vertex_project" json:"vertex_project,omitempty"`
		VertexLocation string `yaml:"vertex_location" json:"vertex_location,omitempty"`
		VertexCredentials any `yaml:"vertex_credentials" json:"vertex_credentials,omitempty"`
		VertexAICredentials any `yaml:"vertex_ai_credentials" json:"vertex_ai_credentials,omitempty"`
		GCSBucket string `yaml:"bucket_name" json:"bucket_name,omitempty"`
		GCSEndpoint string `yaml:"gcs_endpoint" json:"gcs_endpoint,omitempty"`
		VertexDiscoveryEndpoint string `yaml:"vertex_discovery_endpoint" json:"vertex_discovery_endpoint,omitempty"`
		WatsonXProject string `yaml:"watsonx_project" json:"watsonx_project,omitempty"`
		WatsonXSpace string `yaml:"space_id" json:"space_id,omitempty"`
	} `yaml:"litellm_params" json:"litellm_params"`
	Info struct {
		ID        string   `yaml:"id" json:"id,omitempty"`
		Endpoints []string `yaml:"supported_endpoints" json:"supported_endpoints,omitempty"`
		Mode      string   `yaml:"mode" json:"mode,omitempty"`
		DisableBackgroundHealthCheck bool `yaml:"disable_background_health_check" json:"disable_background_health_check,omitempty"`
	} `yaml:"model_info" json:"model_info"`
}

type Config struct {
	azureTokenRefresh bool
	Agents []runtime.Agent
	staticAgents []agentConfig
	publicAgents []string
	Routes    []runtime.Route
	SearchTools []runtime.SearchTool
	MCPServers []runtime.MCPServer
	staticMCPServers map[string]mcpServerConfig
	SearchFallbacks map[string][]string
	staticSearchTools []searchToolConfig
	MasterKey string
	Timeout   time.Duration
	NumRetries int
	MaxFallbacks *int
	RoutingStrategy string
	EnableTagFiltering bool
	TagsMatchAll bool
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
	Callback GenericCallbacks
	LogMessages bool
	publicModelGroups []string
	publicModelLinks map[string]any
	publicMCPServers []string
	ResponsePolling runtime.ResponsePollingConfig
	PollingRedisURL string
	InferenceCache runtime.InferenceCacheConfig
	InferenceCacheRedis *redis.Options
	RoutingRedis *redis.Options
	RoutingRedisNamespace string
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
	if len(file.Models)+len(file.SearchTools)+len(file.General.PassThrough)+len(file.MCPServers)+len(file.Agents) == 0 || file.Router.Timeout < 0 || file.Router.Timeout > 86400 || file.Router.Retries < 0 || file.Router.Retries > 8 {
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
	if file.Router.MaxFallbacks != nil && (*file.Router.MaxFallbacks < 0 || *file.Router.MaxFallbacks > 16) {
		return Config{}, errors.New("invalid max_fallbacks")
	}
	if !file.Router.RetryPolicy.Valid() {
		return Config{}, errors.New("invalid retry_policy")
	}
	for _, policy := range file.Router.ModelRetryPolicies {
		if !policy.Valid() {
			return Config{}, errors.New("invalid model_group_retry_policy")
		}
	}
	config := Config{Timeout: time.Duration(file.Router.Timeout) * time.Second, NumRetries: file.Router.Retries, MaxFallbacks: file.Router.MaxFallbacks,
		RoutingStrategy: strategy, models: file.Models, fallbacks: file.Router.Fallbacks, defaultFallbacks: file.Router.DefaultFallbacks,
		contextFallbacks: file.Router.ContextFallbacks, policyFallbacks: file.Router.PolicyFallbacks,
		LatencyBuffer: args.Buffer, LatencySamples: args.Samples, modelGroupAliases: file.Router.ModelGroupAliases,
		DisableSpendLogs: file.General.DisableSpendLogs, PublicReadinessDetails: file.General.PublicReadinessDetails, AllowedFails: allowedFails,
		Cooldown: time.Duration(cooldownSeconds * float64(time.Second)), RetryPolicy: file.Router.RetryPolicy,
		ModelRetryPolicies: file.Router.ModelRetryPolicies}
	config.EnableTagFiltering = file.Router.EnableTagFiltering
	config.azureTokenRefresh = file.Callbacks.AzureTokenRefresh
	config.TagsMatchAll = file.Router.TagFilteringMatchAny != nil && !*file.Router.TagFilteringMatchAny
	if file.Router.RedisURL != "" || file.Router.RedisHost != "" {
		params := inferenceCacheParams{RedisURL: file.Router.RedisURL, Host: file.Router.RedisHost,
			Port: file.Router.RedisPort, Password: file.Router.RedisPassword}
		options, err := params.redisOptions()
		if err != nil {
			return Config{}, errors.New("invalid router Redis settings")
		}
		config.RoutingRedis = options
		config.RoutingRedisNamespace = file.Router.RedisNamespace
	}
	if file.Callbacks.Cache {
		if err := file.Callbacks.CacheParams.apply(&config); err != nil {
			return Config{}, err
		}
	}
	agents, err := prepareAgents(file.Agents, file.Callbacks.PublicAgentGroups)
	if err != nil { return Config{}, err }
	config.Agents = agents
	config.staticAgents = file.Agents
	config.publicAgents = slices.Clone(file.Callbacks.PublicAgentGroups)
	config.publicModelGroups = slices.Clone(file.Callbacks.PublicModelGroups)
	config.publicModelLinks = file.Callbacks.PublicModelLinks
	config.publicMCPServers = slices.Clone(file.Callbacks.PublicMCPServers)
	polling := file.Callbacks.Responses.Background
	config.ResponsePolling.Native = polling.Native
	if polling.TTL < 0 || polling.TTL > 86400 {
		return Config{}, errors.New("invalid Responses polling TTL")
	}
	config.ResponsePolling.TTL = time.Duration(polling.TTL) * time.Second
	redisURL, err := resolveEnv(polling.RedisURL)
	if err != nil {
		return Config{}, errors.New("invalid Responses polling Redis configuration")
	}
	config.PollingRedisURL = redisURL
	switch enabled := polling.Polling.(type) {
	case nil:
	case bool:
		if enabled {
			return Config{}, errors.New("responses polling_via_cache expects all or a provider list")
		}
	case string:
		if enabled != "all" {
			return Config{}, errors.New("responses polling_via_cache expects all or a provider list")
		}
		config.ResponsePolling.All = true
	case []any:
		for _, provider := range enabled {
			name, ok := provider.(string)
			if !ok || name == "" {
				return Config{}, errors.New("invalid Responses polling provider")
			}
			config.ResponsePolling.Providers = append(config.ResponsePolling.Providers, name)
		}
	default:
		return Config{}, errors.New("invalid Responses polling configuration")
	}
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
		effective.Params.AzureTokenRefresh = effective.Params.AzureTokenRefresh || config.azureTokenRefresh
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
		var chained []string
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
			chained = append(chained, target)
		}
		seen = map[string]bool{alias: true}
		var appendChain func(string) error
		appendChain = func(target string) error {
			if seen[target] || !hasCompatibleDeployment(byAlias[alias], byAlias[target]) {
				return nil
			}
			seen[target] = true
			candidates = append(candidates, byAlias[target]...)
			if len(candidates) > 16 {
				return fmt.Errorf("model %q exceeds the 16-candidate limit", alias)
			}
			nested, exists := fallbacks[target]
			if !exists {
				nested = fallbacks[config.modelGroupAliases[target].Model]
			}
			for _, next := range nested {
				if err := appendChain(next); err != nil {
					return err
				}
			}
			return nil
		}
		for _, target := range chained {
			if err := appendChain(target); err != nil {
				return nil, err
			}
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
	runway := provider == "runwayml"
	pgVector := provider == "pg_vector"
	nativeAudio := provider == "elevenlabs" || provider == "deepgram" || provider == "aws_polly"
	nativeRerank := (provider == "infinity" || provider == "huggingface" || provider == "watsonx") && entry.rerankConfigured()
	_, explicitOCR := strings.CutPrefix(model, "ocr/")
	configuredOCR := entry.Info.Mode == "ocr" || explicitOCR || strings.HasPrefix(model, "mistral-ocr") ||
		strings.Contains(model, "deepseek-ocr") ||
		slices.Contains(entry.Info.Endpoints, "/v1/ocr")
	mistralOCR := provider == "mistral" && configuredOCR
	partnerOCR := configuredOCR && (provider == "vertex_ai" ||
		(provider == "azure_ai" && !strings.Contains(model, "doc-intelligence") && !strings.Contains(model, "documentintelligence")))
	documentIntelligence := provider == "azure_ai" && (strings.Contains(model, "doc-intelligence") || strings.Contains(model, "documentintelligence"))
	if entry.Alias == "" || model == "" || !found || (!supportsOpenAITransport(provider) && provider != "anthropic" &&
		provider != "gemini" && provider != "vertex_ai" && provider != "azure" && provider != "bedrock" && !rerankProvider(provider) && !nativeRerank && !nativeAudio && !mistralOCR && !runway && !pgVector) {
		return runtime.Candidate{}, errors.New("model_name and a supported provider-prefixed model are required")
	}
	if provider == "openai" {
		model, _ = strings.CutPrefix(model, "chat_completions/")
	}
	if mistralOCR || partnerOCR {
		model = strings.TrimPrefix(model, "ocr/")
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
	vertexSpeech := provider == "vertex_ai" && (entry.Info.Mode == "audio_speech" || slices.Contains(entry.Info.Endpoints, "/v1/audio/speech"))
	if vertexSpeech && base == "" {
		base = "https://texttospeech.googleapis.com/v1/text:synthesize"
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
	if base == "" && (provider == "elevenlabs" || provider == "deepgram") {
		if provider == "deepgram" {
			base = os.Getenv("DEEPGRAM_API_BASE")
			if base == "" { base = "https://api.deepgram.com" }
		} else {
			base = os.Getenv("ELEVENLABS_API_BASE")
			if base == "" { base = "https://api.elevenlabs.io" }
		}
	}
	if base == "" && provider == "gemini" {
		base = "https://generativelanguage.googleapis.com"
	}
	if base == "" && mistralOCR {
		base = "https://api.mistral.ai/v1"
	}
	if base == "" && partnerOCR && provider == "azure_ai" {
		base = os.Getenv("AZURE_AI_API_BASE")
	}
	if base == "" && documentIntelligence {
		base = os.Getenv("AZURE_DOCUMENT_INTELLIGENCE_ENDPOINT")
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
		case "azure_ai":
			base = os.Getenv("AZURE_AI_API_BASE")
		case "watsonx":
			base = firstWatsonXEnv("WATSONX_API_BASE", "WATSONX_URL", "WX_URL", "WML_URL")
		case "huggingface":
			base = os.Getenv("HF_API_BASE")
			if base == "" {
				base = os.Getenv("HUGGINGFACE_API_BASE")
			}
			if base == "" {
				base = "https://api-inference.huggingface.co"
			}
		case "nvidia_nim":
			base = os.Getenv("NVIDIA_NIM_API_BASE")
			if base == "" {
				base = "https://ai.api.nvidia.com"
			}
		}
	}
	var bedrock *runtime.BedrockConfig
	if provider == "bedrock" || provider == "aws_polly" {
		if provider == "aws_polly" && entry.Params.AWSRegion == "" && os.Getenv("AWS_REGION") == "" && os.Getenv("AWS_DEFAULT_REGION") == "" {
			entry.Params.AWSRegion = "us-east-1"
		}
		bedrock, err = entry.bedrockConfig()
		if err != nil {
			return runtime.Candidate{}, err
		}
		if base == "" {
			service := "bedrock-runtime"
			if provider == "aws_polly" { service = "polly" }
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
	if pgVector && base == "" {
		base = os.Getenv("PG_VECTOR_API_BASE")
	}
	if runway && base == "" {
		base = "https://api.dev.runwayml.com/v1"
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
	if pgVector && key == "" {
		key = os.Getenv("PG_VECTOR_API_KEY")
		if key == "" {
			return runtime.Candidate{}, errors.New("pg_vector requires api_key or PG_VECTOR_API_KEY")
		}
	}
	if key == "" && runway {
		key = os.Getenv("RUNWAYML_API_SECRET")
		if key == "" {
			key = os.Getenv("RUNWAYML_API_KEY")
		}
	}
	if key == "" && nativeAudio {
		key = os.Getenv(strings.ToUpper(provider)+"_API_KEY")
	}
	if key == "" && hasSettings {
		key = os.Getenv(settings.APIKeyEnv)
	}
	if key == "" && provider == "openai" && parsed.Scheme == "https" && parsed.Host == "api.openai.com" {
		key = os.Getenv("OPENAI_API_KEY")
	}
	if provider == "bedrock" && entry.Params.AWSRole != "" {
		key = ""
	} else if key == "" && provider == "bedrock" {
		key = os.Getenv("AWS_BEARER_TOKEN_BEDROCK")
	}
	if key == "" && mistralOCR {
		key = os.Getenv("MISTRAL_API_KEY")
	}
	if key == "" && partnerOCR && provider == "azure_ai" {
		key = os.Getenv("AZURE_AI_API_KEY")
	}
	if key == "" && documentIntelligence {
		key = os.Getenv("AZURE_DOCUMENT_INTELLIGENCE_API_KEY")
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
		case "azure_ai":
			key = os.Getenv("AZURE_AI_API_KEY")
		case "watsonx":
			key = firstWatsonXEnv("WATSONX_APIKEY", "WATSONX_API_KEY", "WX_API_KEY")
		case "huggingface":
			key = os.Getenv("HUGGINGFACE_API_KEY")
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
	var watsonx *runtime.WatsonXRerankConfig
	if provider == "watsonx" && nativeRerank {
		watsonx, err = entry.watsonXRerankConfig()
		if err != nil {
			return runtime.Candidate{}, err
		}
	}
	var azureADToken, azureIdentityKey string
	var azureToken func(context.Context) (string, error)
	if provider == "azure" {
		azureADToken, err = resolveEnv(entry.Params.AzureADToken)
		if err != nil {
			return runtime.Candidate{}, fmt.Errorf("azure_ad_token: %w", err)
		}
		if azureADToken == "" && key == "" && parsed.Scheme == "https" &&
			strings.HasSuffix(parsed.Hostname(), ".openai.azure.com") {
			azureADToken = os.Getenv("AZURE_AD_TOKEN")
		}
		oidc := ""
		if strings.HasPrefix(azureADToken, "oidc/") {
			oidc, azureADToken = azureADToken, ""
		}
		if key == "" && azureADToken == "" {
			azureToken, azureIdentityKey, err = entry.azureIdentity(parsed.Scheme == "https" && strings.HasSuffix(parsed.Hostname(), ".openai.azure.com"), oidc)
			if err != nil {
				return runtime.Candidate{}, err
			}
		}
	}
	azureSpeech := provider == "azure" && strings.HasPrefix(model, "speech/")
	if provider == "azure" && !azureSpeech && version == "" && !strings.HasSuffix(strings.TrimRight(base, "/"), "/openai/v1") {
		return runtime.Candidate{}, errors.New("Azure deployment API requires api_version")
	}
	endpoint := "chat-completions"
	if pgVector {
		endpoint = "vector-stores"
	}
	if runway {
		endpoint = "videos"
		if version == "" {
			version = os.Getenv("RUNWAYML_DEFAULT_API_VERSION")
		}
	}
	if nativeAudio {
		endpoint = "transcription"
		if provider == "aws_polly" || provider == "elevenlabs" && entry.Info.Mode != "audio_transcription" && !strings.HasPrefix(model, "scribe") {
			endpoint = "speech"
		}
	}
	if mistralOCR {
		endpoint = "ocr"
	}
	if provider == "anthropic" {
		endpoint = "anthropic-messages"
	}
	if provider == "gemini" || provider == "vertex_ai" {
		endpoint = "gemini-generate"
		if entry.Info.Mode == "image_generation" || (entry.Info.Mode == "" && strings.HasPrefix(model, "imagen-")) {
			endpoint = "image-generation"
		}
		if entry.Info.Mode == "image_edit" {
			endpoint = "image-edit"
		}
		if strings.Contains(model, "embedding") || entry.Info.Mode == "embedding" ||
			(provider == "vertex_ai" && (runtime.VertexUsesEmbedContent(model) || strings.Contains(strings.ToLower(model), "bge"))) {
			endpoint = "google-embeddings"
		}
		if provider == "vertex_ai" && (entry.Info.Mode == "rerank" || strings.HasPrefix(model, "semantic-ranker-")) {
			endpoint = "rerank"
		}
	}
	if provider == "bedrock" {
		endpoint = "bedrock-converse"
		if runtime.BedrockSupportsImage(model, "image-generation") {
			endpoint = "image-generation"
		}
		if runtime.BedrockSupportsImage(model, "image-edit") && (endpoint == "bedrock-converse" || entry.Info.Mode == "image_edit") {
			endpoint = "image-edit"
		}
		if strings.Contains(model, "amazon.titan-embed-") || strings.Contains(model, "cohere.embed-") ||
			strings.Contains(model, "amazon.nova-2-multimodal-embeddings") || strings.Contains(model, "twelvelabs.marengo-embed") {
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
	if supportsOpenAITransport(provider) || provider == "azure" || nativeRerank {
		if inferred := openAIModelEndpoint(entry.Info.Mode, model); inferred != "" {
			endpoint = inferred
		}
	}
	if vertexSpeech {
		endpoint = "speech"
	}
	if azureSpeech {
		endpoint = "speech"
		if key == "" { key = os.Getenv("AZURE_SPEECH_KEY") }
	}
	if partnerOCR || documentIntelligence {
		endpoint = "ocr"
	}
	if len(entry.Info.Endpoints) == 1 {
		configured := runtime.EndpointFromPath(entry.Info.Endpoints[0])
		if endpoint == "bedrock-converse" && (configured == "chat-completions" || configured == "responses") {
			configured = endpoint
		}
		if configured == "embeddings" && endpoint == "bedrock-embeddings" {
			configured = endpoint
		}
		if configured == "embeddings" && (provider == "gemini" || provider == "vertex_ai") {
			configured = "google-embeddings"
		}
		if configured == "embeddings" && (provider == "cohere" || provider == "cohere_chat") {
			configured = "cohere-embeddings"
		}
		if nativeAudio && !(provider != "aws_polly" && configured == "transcription") && !((provider == "elevenlabs" || provider == "aws_polly") && configured == "speech") {
			return runtime.Candidate{}, errors.New("unsupported native audio endpoint")
		}
		if configured == "" || (runway && configured != "videos") || (pgVector && configured != "vector-stores") || ((provider == "gemini" || provider == "vertex_ai") && configured != "google-embeddings" && configured != "image-generation" && configured != "image-edit" && !(vertexSpeech && configured == "speech") &&
			!(provider == "vertex_ai" && (configured == "rerank" || (configured == "ocr" && partnerOCR)))) ||
			(configured == "anthropic-messages") != (provider == "anthropic") ||
			(provider == "bedrock" && configured != endpoint && !runtime.BedrockSupportsImage(model, configured)) {
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
	patterns := make([]*regexp.Regexp, 0, len(entry.Params.TagRegex))
	for _, pattern := range entry.Params.TagRegex {
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return runtime.Candidate{}, errors.New("invalid deployment tag_regex")
		}
		patterns = append(patterns, compiled)
	}
	region, err := resolveEnv(entry.Params.Region)
	if err != nil {
		return runtime.Candidate{}, err
	}
	return runtime.Candidate{SourceID: provider, Provider: provider, ModelID: model, Region: region,
		APIBase: base, APIKey: key, Endpoint: endpoint, APIVersion: version, DeploymentID: entry.Info.ID,
		Headers: headers, ParamMappings: settings.ParamMappings, RelativeAPIBase: hasSettings, TextOnly: settings.Special.TextOnly,
		Tags: slices.Clone(entry.Params.Tags), TagRegex: patterns, RoutingGroup: entry.Alias, Weight: entry.Params.Weight, RPM: entry.Params.RPM, TPM: entry.Params.TPM, Bedrock: bedrock,
		DisableBackgroundHealthCheck: entry.Info.DisableBackgroundHealthCheck,
		UseInPassThrough: entry.Params.UseInPassThrough,
		TokenPrice: price, UsagePrice: usagePrice, CacheReadPrice: cachePrices[0], CacheCreationPrice: cachePrices[1],
		Vertex: vertex, AzureADToken: azureADToken, AzureToken: azureToken, AzureIdentityKey: azureIdentityKey, WatsonX: watsonx}, nil
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
		{entry.Params.AWSBedrockControlEndpoint, "AWS_ENDPOINT_URL_BEDROCK", &config.ControlBase},
		{entry.Params.AWSS3VectorsEndpoint, "AWS_ENDPOINT_URL_S3VECTORS", &config.VectorBase},
		{"", "AWS_ENDPOINT_URL_IAM", &config.IAMBase},
		{entry.Params.AWSSTSEndpoint, "AWS_ENDPOINT_URL_STS", &config.STSBase},
		{"", "AWS_ENDPOINT_URL_OPENSEARCHSERVERLESS", &config.OpenSearchBase},
		{entry.Params.AWSBedrockAgentEndpoint, "AWS_ENDPOINT_URL_BEDROCK_AGENT", &config.AgentControlBase},
		{entry.Params.AWSBedrockAgentRuntimeEndpoint, "AWS_ENDPOINT_URL_BEDROCK_AGENT_RUNTIME", &config.AgentBase},
		{entry.Params.AWSBatchRoleARN, "AWS_BATCH_ROLE_ARN", &config.BatchRole},
		{entry.Params.S3OutputBucket, "AWS_S3_OUTPUT_BUCKET_NAME", &config.BatchOutputBucket},
		{entry.Params.S3Bucket, "AWS_S3_BUCKET_NAME", &config.S3Bucket},
		{entry.Params.S3Endpoint, "AWS_ENDPOINT_URL_S3", &config.S3Endpoint},
		{entry.Params.S3Region, "", &config.S3Region},
		{entry.Params.S3EncryptionKey, "AWS_S3_ENCRYPTION_KEY_ID", &config.BatchEncryptionKey},
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
	if entry.Params.AWSRole != "" {
		return entry.assumeAWSRole(config, profile)
	}
	if config.Region == "" || (config.AccessKey == "" && (strings.HasPrefix(entry.Params.Model, "aws_polly/") || (entry.Params.APIKey == "" && os.Getenv("AWS_BEARER_TOKEN_BEDROCK") == ""))) {
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
