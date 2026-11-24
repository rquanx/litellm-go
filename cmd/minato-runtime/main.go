package main

import (
	"context"
	"database/sql"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rquanx/litellm-go/internal/proxyconfig"
	"github.com/rquanx/litellm-go/internal/runtime"
)

func main() {
	management := flag.Bool("management-api", envBool("LITELLM_GO_MANAGEMENT_API", true), "Expose model and access management APIs")
	providerAPIs := flag.Bool("provider-apis", envBool("LITELLM_GO_PROVIDER_APIS", true), "Expose additional provider and resource APIs")
	routeToken := os.Getenv("LITELLM_GO_ROUTE_PUBLICATION_TOKEN")
	configPath := flag.String("config", getenv("LITELLM_GO_CONFIG", os.Getenv("MINATO_RUNTIME_PROXY_CONFIG")), "LiteLLM-compatible YAML configuration")
	statePath := flag.String("model-state", getenv("LITELLM_GO_MODEL_STATE", os.Getenv("MINATO_RUNTIME_MODEL_STATE")), "Optional path for persisted dynamic models")
	keyStatePath := flag.String("key-state", getenv("LITELLM_GO_KEY_STATE", os.Getenv("MINATO_RUNTIME_KEY_STATE")), "Optional path for persisted virtual keys")
	importAgents := flag.Bool("import-litellm-agents", false, "Copy supported LiteLLM agents into Go PostgreSQL state")
	importCredentials := flag.Bool("import-litellm-credentials", false, "Copy supported LiteLLM provider credentials into Go PostgreSQL state")
	importModels := flag.Bool("import-litellm-models", false, "Copy LiteLLM models into empty Go proxy PostgreSQL state and exit")
	importKeys := flag.Bool("import-litellm-keys", false, "Copy supported LiteLLM key digests into empty Go proxy state and exit")
	importAccess := flag.Bool("import-litellm-access", false, "Import supported LiteLLM access records and keys into empty Go PostgreSQL state")
	importSearch := flag.Bool("import-litellm-search-tools", false, "Import LiteLLM search tools into Go PostgreSQL state")
	importMCP := flag.Bool("import-litellm-mcp-servers", false, "Import supported LiteLLM MCP servers into Go PostgreSQL state")
	importSpend := flag.Bool("import-litellm-spend", false, "Import LiteLLM spend metadata into Go PostgreSQL state")
	importCache := flag.Bool("import-litellm-cache", false, "Copy supported LiteLLM cache settings into Go PostgreSQL state")
	importHealth := flag.Bool("import-litellm-health", false, "Import LiteLLM health history into Go PostgreSQL state")
	port := flag.Int("port", getint("LITELLM_GO_PORT", getint("MINATO_RUNTIME_PORT", 4000)), "HTTP listener port")
	host := flag.String("host", "0.0.0.0", "HTTP listener host")
	flag.Parse()
	importCount := 0
	for _, requested := range []bool{*importModels, *importAgents, *importCredentials, *importKeys, *importAccess, *importSearch, *importMCP, *importSpend, *importHealth, *importCache} {
		if requested {
			importCount++
		}
	}
	compatibilityUpstream := os.Getenv("LITELLM_GO_COMPATIBILITY_UPSTREAM")
	nativePaths := os.Getenv("LITELLM_GO_NATIVE_PATHS")
	if importCount == 0 && compatibilityUpstream != "" && strings.Trim(nativePaths, " ,\t\r\n") == "" {
		handler, err := withCompatibilityUpstream(nil, compatibilityUpstream, nativePaths)
		if err != nil {
			log.Fatal(err)
		}
		serve(*host, *port, handler)
		return
	}
	config := runtime.Config{
		Token: os.Getenv("LITELLM_MASTER_KEY"),
		AuthorizeInference: true,
		DisableProviderAPIs: !*providerAPIs,
		RequireInferenceAuth: true,
		DisableManagementAPI: !*management,
		RoutePublicationToken: routeToken,
		HTTPClient:    &http.Client{Timeout: 120 * time.Second},
		MaxBodyBytes:  8 << 20,
		MaxConcurrent: getint("LITELLM_GO_MAX_CONCURRENT", getint("MINATO_RUNTIME_MAX_CONCURRENT", 128)),
	}
	var proxy proxyconfig.Config
	if *configPath != "" {
		var err error
		proxy, err = proxyconfig.LoadFile(*configPath)
		if err != nil {
			log.Fatal(err)
		}
		config.Routes = proxy.Routes
		config.DropParams = proxy.DropParams
		config.DisableUserAgentTags = proxy.DisableUserAgentTags
		config.RequestDefaults = proxy.RequestDefaults
		config.AllowedIPs = proxy.AllowedIPs
		config.SearchTools = proxy.SearchTools
		config.MCPServers = proxy.MCPServers
		config.Agents = proxy.Agents
		config.SearchFallbacks = proxy.SearchFallbacks
		config.PassThrough = proxy.PassThrough
		config.NumRetries = proxy.NumRetries
		config.RetryAfter = proxy.RetryAfter
		config.StreamTimeout = proxy.StreamTimeout
		config.MaxFallbacks = proxy.MaxFallbacks
		config.RoutingStrategy = proxy.RoutingStrategy
		config.EnableTagFiltering = proxy.EnableTagFiltering
		config.TagsMatchAll = proxy.TagsMatchAll
		config.LatencyBuffer = proxy.LatencyBuffer
		config.LatencySamples = proxy.LatencySamples
		config.AllowedFails = proxy.AllowedFails
		config.Cooldown = proxy.Cooldown
		config.RetryPolicy = proxy.RetryPolicy
		config.ModelRetryPolicies = proxy.ModelRetryPolicies
		if proxy.MasterKey != "" {
			config.Token = proxy.MasterKey
		}
		config.PublicReadinessDetails = proxy.PublicReadinessDetails
		config.AlwaysIncludeStreamUsage = proxy.AlwaysIncludeStreamUsage
		config.ResponsePolling = proxy.ResponsePolling
		config.InferenceCache = proxy.InferenceCache
		config.BackgroundHealthInterval = proxy.BackgroundHealthInterval
		config.SharedHealthChecks = proxy.SharedHealthChecks
		config.Guardrails = proxy.Guardrails
		config.HealthCheckConcurrency = proxy.HealthCheckConcurrency
		config.SkipDisabledHealthChecks = proxy.SkipDisabledHealthChecks
		config.HealthRoutingStaleness = proxy.HealthRoutingStaleness
		config.IgnoreTransientHealthErrors = proxy.IgnoreTransientHealthErrors
		if proxy.Timeout > 0 {
			config.HTTPClient.Timeout = proxy.Timeout
		}
	}
	if config.Token == "" {
		log.Fatal("set LITELLM_MASTER_KEY or general_settings.master_key")
	}
	proxy.MasterKey = config.Token
	var stateDB *sql.DB
	if proxy.DatabaseURL != "" {
		if *statePath != "" || *keyStatePath != "" || config.Token == "" {
			log.Fatal("PostgreSQL state requires a master key and separate storage from local state flags")
		}
		var err error
		stateDB, err = proxyconfig.OpenPostgresState(proxy.DatabaseURL)
		if err != nil {
			log.Fatal(err)
		}
		defer stateDB.Close()
		config.DatabasePing = stateDB.PingContext
		config.LegacyFiles = proxyconfig.LegacyFileLookup(stateDB)
		config.LegacyVectorStores = proxyconfig.LegacyVectorStoreLookup(stateDB)
	}
	if importCount != 0 {
		if stateDB == nil {
			log.Fatal("LiteLLM import requires general_settings.database_url")
		}
		if importCount != 1 {
			log.Fatal("Select one LiteLLM import command")
		}
		if *importCache {
			imported, err := proxyconfig.ImportLiteLLMCache(stateDB, config.Token)
			if err != nil {
				log.Fatal(err)
			}
			log.Printf("Imported LiteLLM cache settings: %t", imported)
			return
		}
		if *importHealth {
			count, err := proxyconfig.ImportLiteLLMHealth(stateDB)
			if err != nil {
				log.Fatal(err)
			}
			log.Printf("Imported %d LiteLLM health checks", count)
			return
		}
		if *importSpend {
			count, err := proxyconfig.ImportLiteLLMSpend(stateDB)
			if err != nil {
				log.Fatal(err)
			}
			log.Printf("Imported %d LiteLLM spend records", count)
			return
		}
		if *importSearch {
			count, err := proxyconfig.ImportLiteLLMSearchTools(stateDB, proxy)
			if err != nil {
				log.Fatal(err)
			}
			log.Printf("Imported %d LiteLLM search tools", count)
			return
		}
		if *importMCP {
			count, err := proxyconfig.ImportLiteLLMMCPServers(stateDB, proxy)
			if err != nil {
				log.Fatal(err)
			}
			log.Printf("Imported %d LiteLLM MCP servers", count)
			return
		}
		if *importAccess {
			var sourceRedis *redis.Client
			if source := os.Getenv("LITELLM_GO_IMPORT_REDIS_URL"); source != "" {
				options, err := redis.ParseURL(source)
				if err != nil {
					log.Fatal("invalid LITELLM_GO_IMPORT_REDIS_URL")
				}
				sourceRedis = redis.NewClient(options)
				defer sourceRedis.Close()
			}
			count, err := proxyconfig.ImportLiteLLMAccessWithRedis(stateDB, sourceRedis, os.Getenv("LITELLM_GO_IMPORT_REDIS_NAMESPACE"))
			if err != nil {
				log.Fatal(err)
			}
			log.Printf("Imported LiteLLM access records and %d virtual keys", count)
			return
		}
		if *importKeys {
			count, err := proxyconfig.ImportLiteLLMKeys(stateDB)
			if err != nil {
				log.Fatal(err)
			}
			log.Printf("Imported %d LiteLLM virtual keys", count)
			return
		}
		if *importAgents {
			count, err := proxyconfig.ImportLiteLLMAgents(stateDB, proxy)
			if err != nil {
				log.Fatal(err)
			}
			log.Printf("Imported %d LiteLLM agents", count)
			return
		}
		if *importCredentials {
			count, err := proxyconfig.ImportLiteLLMCredentials(stateDB, proxy)
			if err != nil {
				log.Fatal(err)
			}
			log.Printf("Imported %d LiteLLM provider credentials", count)
			return
		}
		count, err := proxyconfig.ImportLiteLLMModels(stateDB, proxy)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("Imported %d LiteLLM deployments", count)
		return
	}
	var keys *proxyconfig.KeyRegistry
	var err error
	if stateDB != nil {
		keys, err = proxyconfig.LoadPostgresKeys(config.Token, stateDB)
	} else {
		keys, err = proxyconfig.LoadKeys(config.Token, *keyStatePath)
	}
	if err != nil {
		log.Fatal(err)
	}
	config.AuthorizeKey = keys.Authorize
	config.AuthorizePassThrough = keys.AuthorizePassThrough
	keys.SetConfiguredModelAccessGroups(proxy)
	config.AuthorizeSearch = keys.AuthorizeSearch
	config.AuthorizeAgent = keys.AuthorizeAgent
	config.AuthorizeMCP = keys.AuthorizeMCP
	config.AuthorizeMCPToolset = keys.AuthorizeMCPToolset
	config.ResourceOwnerScopes = keys.ContainerOwnerScopes
	if stateDB != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		history, err := proxyconfig.OpenHealthHistory(ctx, stateDB)
		if err != nil {
			cancel()
			log.Fatal(err)
		}
		config.HealthHistory = history
		owners, err := proxyconfig.OpenContainerOwners(ctx, stateDB)
		cancel()
		if err != nil {
			log.Fatal(err)
		}
		config.ContainerOwners = owners
	}
	costSettings, err := proxy.OpenCostSettings(stateDB, config.Token)
	if err != nil {
		log.Fatal(err)
	}
	proxy.CostAdjustment = costSettings
	if proxy.Callback != nil {
		config.Callback = proxy.Callback.Start(context.Background())
		config.CallbackNames = proxy.Callback.Names()
		config.CallbackHealth = proxy.Callback.Probe
		config.LogMessages = proxy.LogMessages
	}
	var spend *proxyconfig.SpendLog
	var assistantRunStore *proxyconfig.AssistantRunStore
	keys.SetBudgetTimezone(proxy.BudgetTimezone)
	config.CheckKeyBudget = keys.CheckKeyBudgetWithoutStore
	if len(proxy.ProviderBudgets) > 0 && (stateDB == nil || proxy.DisableSpendLogs) {
		log.Fatal("router_settings.provider_budget_config requires PostgreSQL spend tracking")
	}
	if proxy.GlobalMaxBudget > 0 && (stateDB == nil || proxy.DisableSpendLogs) {
		log.Fatal("litellm_settings.max_budget requires PostgreSQL spend tracking")
	}
	if stateDB != nil && !proxy.DisableSpendLogs {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var err error
		spend, err = proxyconfig.OpenSpendLog(ctx, stateDB, keys)
		if err == nil && proxy.GlobalMaxBudget > 0 {
			err = spend.ConfigureGlobalBudget(ctx, proxy)
			config.CheckGlobalBudget = spend.CheckGlobalBudget
		}
		cancel()
		if err != nil {
			log.Fatal(err)
		}
		if len(proxy.ProviderBudgets) > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := spend.ConfigureProviderBudgets(ctx, proxy)
			cancel()
			if err != nil {
				log.Fatal(err)
			}
			config.CheckProviderBudget = spend.CheckProviderBudget
		}
		if *providerAPIs {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			assistantRunStore, err = spend.OpenAssistantRunStore(ctx, config.Token, costSettings)
			cancel()
			if err != nil {
				log.Fatal(err)
			}
			config.AssistantRuns = assistantRunStore
		}
		storePayloads := proxy.StoreSpendPayloads || envBool("STORE_PROMPTS_IN_SPEND_LOGS", false)
		spend.SetStorePayloads(storePayloads)
		config.LogMessages = config.LogMessages || storePayloads
		config.CaptureSpendStream = storePayloads
		record := spend.Start(context.Background())
		config.CheckKeyBudget = spend.CheckKeyBudget
		config.CheckTagBudget = spend.CheckTagBudget
		callback := config.Callback
		config.Callback = func(event runtime.InferenceEvent) {
			record(event)
			if callback != nil {
				event.SpendStreamResponse = nil
				if !proxy.LogMessages {
					event.Messages = nil
					event.Response = nil
				}
				callback(event)
			}
		}
	}
	if callback := config.Callback; callback != nil {
		config.Callback = func(event runtime.InferenceEvent) { callback(costSettings.AdjustEvent(event)) }
	}
	if teamCallback := keys.TeamCallback(context.Background()); teamCallback != nil {
		callback := config.Callback
		config.Callback = func(event runtime.InferenceEvent) {
			if callback != nil {
				callback(event)
			}
			teamCallback(event)
		}
	}
	if proxy.PollingRedisURL != "" && (config.ResponsePolling.All || len(config.ResponsePolling.Providers) > 0) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cache, err := proxyconfig.OpenRedisPollingStore(ctx, proxy.PollingRedisURL)
		cancel()
		if err != nil {
			log.Fatal(err)
		}
		defer cache.Close()
		config.PollingStore = cache
	}
	if proxy.InferenceCacheRedis != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cache, err := proxyconfig.OpenRedisInferenceCache(ctx, proxy.InferenceCacheRedis, proxy.InferenceCache.Namespace)
		cancel()
		if err != nil {
			log.Fatal(err)
		}
		defer cache.Close()
		config.InferenceCacheStore = cache
	}
	if proxy.RoutingRedis != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		state, err := proxyconfig.OpenRedisRoutingState(ctx, proxy.RoutingRedis, proxy.RoutingRedisNamespace)
		cancel()
		if err != nil { log.Fatal(err) }
		defer state.Close()
		config.RoutingState = state
		if config.SharedHealthChecks {
			config.HealthCoordinator = state
		}
	}
	var customers *proxyconfig.CustomerRegistry
	if stateDB != nil && keys != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var err error
		customers, err = proxyconfig.OpenCustomers(ctx, stateDB, keys)
		cancel()
		if err != nil {
			log.Fatal(err)
		}
		config.CustomerPolicy = customers.Policy
		config.ResolveDeploymentTags = customers.ResolveDeploymentTags
	}
	if stateDB != nil && keys != nil {
		config.VectorRegistryLookup = proxyconfig.VectorRegistryLookup(stateDB, keys)
		config.MilvusIndexLookup = proxyconfig.MilvusIndexLookup(stateDB, keys)
		credentials, err := proxyconfig.OpenMCPUserCredentialStore(stateDB, config.Token)
		if err != nil {
			log.Fatal(err)
		}
		config.MCPUserOAuthStore = credentials
		config.MCPUserOAuthIdentity = keys.MCPUserIdentity
	}
	policies, err := proxyconfig.OpenPolicyRegistry(proxy.Policy, stateDB)
	if err != nil {
		log.Fatal(err)
	}
	config.ResolvePolicy = policies.Resolver(keys, config.Token)
	config.ResolvePrompt = proxy.PromptResolver(stateDB, keys)
	var guardrailUsage *proxyconfig.GuardrailUsage
	if keys != nil && stateDB != nil {
		config.GuardrailTeam = keys.GuardrailTeam
		guardrailUsage,err = proxyconfig.OpenGuardrailUsage(stateDB,keys)
		if err!=nil {log.Fatal(err)}
		config.RecordGuardrail=guardrailUsage.Start(context.Background())
	}
	server := runtime.NewServer(config)
	var handler http.Handler = server
	if *management || stateDB != nil || *statePath != "" {
		var managed http.Handler
		if stateDB != nil {
			managed, err = proxyconfig.WithPostgresModelManagement(context.Background(), server, proxy, config.Token, stateDB, keys)
		} else {
			managed, err = proxyconfig.WithModelManagement(server, proxy, config.Token, *statePath, keys)
		}
		if err != nil {
			log.Fatal(err)
		}
		managed, err = proxyconfig.WithAgentManagement(managed, server, proxy, stateDB, *statePath)
		if err != nil {
			log.Fatal(err)
		}
		if *management {
			handler = managed
		}
	}
	if stateDB != nil {
		guardrailRegistry, err := proxyconfig.WithGuardrailRegistry(handler, server, keys, stateDB, proxy.Guardrails)
		if err != nil {
			log.Fatal(err)
		}
		if *management {
			handler = guardrailRegistry
		}
		go guardrailRegistry.RunRefresh(context.Background())
	}
	if *management {
		handler, err = proxyconfig.WithSkillMarketplace(handler, stateDB, keys)
		if err != nil {
			log.Fatal(err)
		}
		if stateDB != nil {
			handler, err = proxyconfig.WithBudgetManagement(handler, stateDB, keys, proxy.BudgetTimezone)
			if err != nil {
				log.Fatal(err)
			}
		}
		if stateDB != nil && keys != nil {
			var err error
			handler, err = proxyconfig.WithVectorRegistry(handler, stateDB, keys)
			if err != nil {
				log.Fatal(err)
			}
			handler, err = proxyconfig.WithMemoryManagement(handler, stateDB, keys)
			if err != nil { log.Fatal(err) }

		}
		if keys != nil {
			handler, err = proxyconfig.WithWorkflowManagement(handler, stateDB, keys)
			if err != nil {
				log.Fatal(err)
			}
			handler,err=proxy.WithPromptManagement(handler,stateDB,keys)
			if err!=nil {log.Fatal(err)}
			keys.SetSSOUserDefaults(proxy.SSOUserDefaults,proxy.SSOTeamClaim)
			keys.SetSSOTeamDefaults(proxy.SSOTeamDefaults)
			keys.SetTeamMemberSpend(spend)
			handler = keys.Handler(handler)
			handler = proxy.Callback.ManagementHandler(handler, keys)
			handler = costSettings.Handler(handler, keys)
		}
		handler = policies.Handler(handler, config.Token, keys, server)
		if guardrailUsage!=nil {
			handler=guardrailUsage.Handler(handler,proxy.Guardrails)
		}
		if customers != nil {
			handler = customers.Handler(handler)
			handler = proxyconfig.WithTagManagement(handler, stateDB, keys, proxy.BudgetTimezone, spend != nil)
		}
		if spend != nil {
			handler = spend.Handler(handler, server)
		}
	}
	if *management && stateDB != nil {
		if err := proxyconfig.InitManagementAudit(context.Background(), stateDB); err != nil {
			log.Fatal(err)
		}
		keys.SetStoreAuditLogs(proxy.StoreAuditLogs)
		handler = proxyconfig.WithManagementAudit(handler, stateDB, keys)
	}
	if keys != nil && (*management || stateDB != nil) {
		cacheSettings, err := proxy.WithCacheSettings(handler, keys, stateDB, server)
		if err != nil {
			log.Fatal(err)
		}
		if *management {
			handler = cacheSettings
		}
	}
	go keys.RunRefresh(context.Background())
	if customers != nil && !proxy.DisableResetBudget {
		go customers.RunBudgetResets(context.Background(), proxy)
	}
	if stateDB != nil {
		go policies.RunRefresh(context.Background())
	}
	if config.BackgroundHealthInterval > 0 {
		go server.RunBackgroundHealthChecks(context.Background())
	}
	if *management {
		handler = proxyconfig.WithDashboardLogin(handler, config.Token, keys)
		if directory := os.Getenv("LITELLM_GO_DASHBOARD_DIR"); directory != "" && !envBool("DISABLE_ADMIN_UI", false) {
			root, err := os.OpenRoot(directory)
			if err != nil {
				log.Fatal("could not open the Dashboard assets directory")
			}
			defer root.Close()
			handler = withDashboardAssets(handler, root)
		}
		handler = proxy.WithDashboardTheme(handler, keys, stateDB)
		handler = proxy.WithDashboardPreferences(handler, keys, stateDB, server, *providerAPIs)
	}
	if assistantRunStore != nil {
		go assistantRunStore.Run(context.Background(), server)
	}
	handler = withNativeOpenAPI(handler, *providerAPIs, *management)
	handler, err = withCompatibilityUpstream(handler, compatibilityUpstream, nativePaths)
	if err != nil {
		log.Fatal(err)
	}
	handler = withLogoUpload(handler, config.Token, &logoUploadStore{})
	handler = withRobots(handler)
	serve(*host, *port, handler)
}

func serve(host string, port int, handler http.Handler) {
	address := net.JoinHostPort(host, strconv.Itoa(port))
	log.Printf("litellm-go listening on %s", address)
	if err := http.ListenAndServe(address, handler); err != nil {
		log.Fatal(err)
	}
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getint(key string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(key))
	if err != nil || value < 1 {
		return fallback
	}
	return value
}

func envBool(key string, fallback bool) bool {
	raw, present := os.LookupEnv(key)
	if !present {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		log.Fatalf("%s must be a boolean", key)
	}
	return value
}
