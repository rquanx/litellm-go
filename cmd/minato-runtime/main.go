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
	"time"

	"github.com/rquanx/minato-runtime/internal/proxyconfig"
	"github.com/rquanx/minato-runtime/internal/runtime"
)

func main() {
	configPath := flag.String("config", os.Getenv("MINATO_RUNTIME_PROXY_CONFIG"), "LiteLLM-compatible YAML configuration")
	statePath := flag.String("model-state", os.Getenv("MINATO_RUNTIME_MODEL_STATE"), "Optional path for persisted dynamic models")
	keyStatePath := flag.String("key-state", os.Getenv("MINATO_RUNTIME_KEY_STATE"), "Optional path for persisted virtual keys")
	importAgents := flag.Bool("import-litellm-agents", false, "Copy supported LiteLLM agents into Go PostgreSQL state")
	importCredentials := flag.Bool("import-litellm-credentials", false, "Copy supported LiteLLM provider credentials into Go PostgreSQL state")
	importModels := flag.Bool("import-litellm-models", false, "Copy LiteLLM models into empty Go proxy PostgreSQL state and exit")
	importKeys := flag.Bool("import-litellm-keys", false, "Copy supported LiteLLM key digests into empty Go proxy state and exit")
	importAccess := flag.Bool("import-litellm-access", false, "Import supported LiteLLM access records and keys into empty Go PostgreSQL state")
	importSearch := flag.Bool("import-litellm-search-tools", false, "Import LiteLLM search tools into Go PostgreSQL state")
	importMCP := flag.Bool("import-litellm-mcp-servers", false, "Import supported LiteLLM MCP servers into Go PostgreSQL state")
	importSpend := flag.Bool("import-litellm-spend", false, "Import LiteLLM spend metadata into Go PostgreSQL state")
	importHealth := flag.Bool("import-litellm-health", false, "Import LiteLLM health history into Go PostgreSQL state")
	port := flag.Int("port", getint("MINATO_RUNTIME_PORT", 4300), "HTTP listener port")
	host := flag.String("host", "0.0.0.0", "HTTP listener host")
	flag.Parse()
	config := runtime.Config{
		Token:         getenv("MINATO_RUNTIME_TOKEN", ""),
		HTTPClient:    &http.Client{Timeout: 120 * time.Second},
		MaxBodyBytes:  8 << 20,
		MaxConcurrent: getint("MINATO_RUNTIME_MAX_CONCURRENT", 128),
	}
	var proxy proxyconfig.Config
	if *configPath != "" {
		file, err := os.Open(*configPath)
		if err != nil {
			log.Fatal(err)
		}
		proxy, err = proxyconfig.Load(file)
		file.Close()
		if err != nil {
			log.Fatal(err)
		}
		config.Routes = proxy.Routes
		config.SearchTools = proxy.SearchTools
		config.MCPServers = proxy.MCPServers
		config.Agents = proxy.Agents
		config.SearchFallbacks = proxy.SearchFallbacks
		config.PassThrough = proxy.PassThrough
		config.NumRetries = proxy.NumRetries
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
		proxy.MasterKey = config.Token
		config.AuthorizeInference = true
		config.PublicReadinessDetails = proxy.PublicReadinessDetails
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
	importCount := 0
	for _, requested := range []bool{*importModels, *importAgents, *importCredentials, *importKeys, *importAccess, *importSearch, *importMCP, *importSpend, *importHealth} {
		if requested {
			importCount++
		}
	}
	if importCount != 0 {
		if stateDB == nil {
			log.Fatal("LiteLLM import requires general_settings.database_url")
		}
		if importCount != 1 {
			log.Fatal("Select one LiteLLM import command")
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
			count, err := proxyconfig.ImportLiteLLMAccess(stateDB)
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
	if *configPath != "" && config.Token != "" {
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
		config.AuthorizeSearch = keys.AuthorizeSearch
		config.AuthorizeAgent = keys.AuthorizeAgent
		config.AuthorizeMCP = keys.AuthorizeMCP
		config.AuthorizeMCPToolset = keys.AuthorizeMCPToolset
		config.ResourceOwnerScopes = keys.ContainerOwnerScopes
	}
	if *keyStatePath != "" && keys == nil {
		log.Fatal("virtual key state requires standalone config and a master key")
	}
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
	if proxy.Callback != nil {
		config.Callback = proxy.Callback.Start(context.Background())
		config.CallbackNames = proxy.Callback.Names()
		config.CallbackHealth = proxy.Callback.Probe
		config.LogMessages = proxy.LogMessages
	}
	var spend *proxyconfig.SpendLog
	if stateDB != nil && !proxy.DisableSpendLogs {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var err error
		spend, err = proxyconfig.OpenSpendLog(ctx, stateDB, keys)
		cancel()
		if err != nil {
			log.Fatal(err)
		}
		record := spend.Start(context.Background())
		callback := config.Callback
		config.Callback = func(event runtime.InferenceEvent) {
			record(event)
			if callback != nil {
				callback(event)
			}
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
	}
	if stateDB != nil && keys != nil {
		config.VectorRegistryLookup = proxyconfig.VectorRegistryLookup(stateDB, keys)
		config.MilvusIndexLookup = proxyconfig.MilvusIndexLookup(stateDB, keys)
		config.ResolvePrompt = proxyconfig.StoredPromptResolver(stateDB, keys)
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
	if keys != nil && stateDB != nil {
		config.GuardrailTeam = keys.GuardrailTeam
	}
	server := runtime.NewServer(config)
	var handler http.Handler = server
	if *configPath != "" {
		var err error
		if stateDB != nil {
			handler, err = proxyconfig.WithPostgresModelManagement(server, proxy, config.Token, stateDB)
		} else {
			handler, err = proxyconfig.WithModelManagement(server, proxy, config.Token, *statePath)
		}
		if err != nil {
			log.Fatal(err)
		}
		handler, err = proxyconfig.WithAgentManagement(handler, server, proxy, stateDB, *statePath)
		if err != nil {
			log.Fatal(err)
		}
	}
	if *configPath != "" {
		var err error
		handler, err = proxyconfig.WithSkillMarketplace(handler, stateDB, keys)
		if err != nil { log.Fatal(err) }
	}
	if stateDB != nil && keys != nil {
		var err error
		handler, err = proxyconfig.WithVectorRegistry(handler, stateDB, keys)
		if err != nil {
			log.Fatal(err)
		}
		handler, err = proxyconfig.WithMemoryManagement(handler, stateDB, keys)
		if err != nil { log.Fatal(err) }
		handler, err = proxyconfig.WithPromptManagement(handler, stateDB, keys)
		if err != nil { log.Fatal(err) }
		guardrailRegistry, err := proxyconfig.WithGuardrailRegistry(handler, server, keys, stateDB, proxy.Guardrails)
		if err != nil { log.Fatal(err) }
		handler = guardrailRegistry
		go guardrailRegistry.RunRefresh(context.Background())
	}
	if keys != nil {
		keys.SetSSOUserDefaults(proxy.SSOUserDefaults)
		keys.SetTeamMemberSpend(spend)
		handler = keys.Handler(handler)
	}
	handler = policies.Handler(handler, config.Token, keys)
	if customers != nil {
		handler = customers.Handler(handler)
	}
	if spend != nil {
		handler = spend.Handler(handler, server)
	}
	address := net.JoinHostPort(*host, strconv.Itoa(*port))
	if stateDB != nil {
		go policies.RunRefresh(context.Background())
	}
	if config.BackgroundHealthInterval > 0 {
		go server.RunBackgroundHealthChecks(context.Background())
	}
	log.Printf("minato-runtime listening on %s", address)
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
