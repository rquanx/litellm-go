package proxyconfig

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/rquanx/litellm-go/internal/runtime"
)

type searchToolConfig struct {
	Name   string `yaml:"search_tool_name" json:"search_tool_name"`
	Params struct {
		Provider string  `yaml:"search_provider" json:"search_provider"`
		APIKey   string  `yaml:"api_key" json:"api_key,omitempty"`
		APIBase  string  `yaml:"api_base" json:"api_base,omitempty"`
		EngineID string  `yaml:"search_engine_id" json:"search_engine_id,omitempty"`
		Timeout  float64 `yaml:"timeout" json:"timeout,omitempty"`
		Retries  *int    `yaml:"max_retries" json:"max_retries,omitempty"`
	} `yaml:"litellm_params" json:"litellm_params"`
	Info struct {
		Description string `yaml:"description" json:"description,omitempty"`
	} `yaml:"search_tool_info" json:"search_tool_info"`
}

var searchProviderNames = map[string]string{
	"perplexity": "Perplexity",
	"tavily": "Tavily",
	"exa_ai": "Exa AI",
	"linkup": "Linkup",
	"firecrawl": "Firecrawl",
	"serper": "Serper",
	"searxng": "SearXNG",
	"parallel_ai": "Parallel AI",
	"searchapi": "SearchAPI.io (Google Search)",
	"google_pse": "Google PSE",
	"dataforseo": "DataForSEO",
	"brave": "Brave Search",
	"duckduckgo": "DuckDuckGo",
}

func (config Config) searchFallbacks() map[string][]string {
	names := make(map[string]bool, len(config.SearchTools))
	for _, tool := range config.SearchTools {
		names[tool.Name] = true
	}
	configured := make(map[string][]string)
	for _, rule := range config.fallbacks {
		for name, targets := range rule {
			configured[name] = targets
		}
	}
	fallbacks := make(map[string][]string, len(names))
	for name := range names {
		targets, explicit := configured[name]
		if !explicit {
			targets = configured["*"]
		}
		seen := map[string]bool{name: true}
		for _, group := range [][]string{targets, config.defaultFallbacks} {
			for _, target := range group {
				if names[target] && !seen[target] {
					fallbacks[name] = append(fallbacks[name], target)
					seen[target] = true
				}
			}
		}
	}
	return fallbacks
}

func prepareSearchTools(entries []searchToolConfig) ([]runtime.SearchTool, error) {
	tools := make([]runtime.SearchTool, 0, len(entries))
	for _, entry := range entries {
		tool, err := entry.prepare()
		if err != nil {
			return nil, fmt.Errorf("search tool %q: %w", entry.Name, err)
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func (entry searchToolConfig) prepare() (runtime.SearchTool, error) {
	params := entry.Params
	if strings.TrimSpace(entry.Name) == "" || entry.Name == "*" || strings.Contains(entry.Name, "/") ||
		params.Timeout < 0 || params.Timeout > 86400 || math.IsNaN(params.Timeout) ||
		(params.Retries != nil && (*params.Retries < 0 || *params.Retries > 8)) {
		return runtime.SearchTool{}, fmt.Errorf("invalid name, timeout or max_retries")
	}
	var defaultBase, baseEnv, keyEnv string
	switch params.Provider {
	case "perplexity":
		defaultBase, baseEnv, keyEnv = "https://api.perplexity.ai", "PERPLEXITY_API_BASE", "PERPLEXITYAI_API_KEY"
	case "tavily":
		defaultBase, baseEnv, keyEnv = "https://api.tavily.com", "TAVILY_API_BASE", "TAVILY_API_KEY"
	case "exa_ai":
		defaultBase, baseEnv, keyEnv = "https://api.exa.ai", "EXA_API_BASE", "EXA_API_KEY"
	case "linkup":
		defaultBase, baseEnv, keyEnv = "https://api.linkup.so/v1", "LINKUP_API_BASE", "LINKUP_API_KEY"
	case "firecrawl":
		defaultBase, baseEnv, keyEnv = "https://api.firecrawl.dev/v2", "FIRECRAWL_API_BASE", "FIRECRAWL_API_KEY"
	case "serper":
		defaultBase, baseEnv, keyEnv = "https://google.serper.dev", "SERPER_API_BASE", "SERPER_API_KEY"
	case "searxng":
		baseEnv, keyEnv = "SEARXNG_API_BASE", "SEARXNG_API_KEY"
	case "parallel_ai":
		defaultBase, baseEnv, keyEnv = "https://api.parallel.ai", "PARALLEL_AI_API_BASE", "PARALLEL_AI_API_KEY"
	case "searchapi":
		defaultBase, baseEnv, keyEnv = "https://www.searchapi.io/api/v1/search", "SEARCHAPI_API_BASE", "SEARCHAPI_API_KEY"
	case "google_pse":
		defaultBase, baseEnv, keyEnv = "https://www.googleapis.com/customsearch/v1", "GOOGLE_PSE_API_BASE", "GOOGLE_PSE_API_KEY"
	case "dataforseo":
		defaultBase, baseEnv = "https://api.dataforseo.com/v3/serp/google/organic/live/advanced", "DATAFORSEO_API_BASE"
	case "brave":
		defaultBase, baseEnv, keyEnv = "https://api.search.brave.com/res/v1/web/search", "BRAVE_API_BASE", "BRAVE_API_KEY"
	case "duckduckgo":
		defaultBase, baseEnv = "https://api.duckduckgo.com", "DUCKDUCKGO_API_BASE"
	default:
		return runtime.SearchTool{}, fmt.Errorf("unsupported search_provider %q", params.Provider)
	}
	base, err := resolveEnv(params.APIBase)
	if err != nil {
		return runtime.SearchTool{}, err
	}
	if base == "" {
		base = os.Getenv(baseEnv)
	}
	if base == "" {
		base = defaultBase
	}
	target, err := url.Parse(base)
	if err != nil || target.Hostname() == "" || (target.Scheme != "http" && target.Scheme != "https") ||
		target.User != nil || target.Fragment != "" {
		return runtime.SearchTool{}, fmt.Errorf("invalid search API base")
	}
	suffix := "/search"
	if params.Provider == "parallel_ai" {
		suffix = "/v1beta/search"
	}
	switch params.Provider {
	case "searchapi", "google_pse", "dataforseo", "brave":
	case "duckduckgo":
		target.Path = strings.TrimRight(target.Path, "/") + "/"
	default:
		if !strings.HasSuffix(strings.TrimRight(target.Path, "/"), suffix) {
			target = target.JoinPath(suffix)
		}
	}
	key, err := resolveEnv(params.APIKey)
	if err != nil {
		return runtime.SearchTool{}, err
	}
	if key == "" {
		key = os.Getenv(keyEnv)
	}
	if key == "" && params.Provider == "parallel_ai" {
		key = os.Getenv("PARALLEL_API_KEY")
	}
	var engineID string
	var location int
	switch params.Provider {
	case "google_pse":
		engineID, err = resolveEnv(params.EngineID)
		if err != nil {
			return runtime.SearchTool{}, err
		}
		if engineID == "" {
			engineID = os.Getenv("GOOGLE_PSE_ENGINE_ID")
		}
		if engineID == "" {
			return runtime.SearchTool{}, fmt.Errorf("missing Google PSE engine ID")
		}
	case "dataforseo":
		login, password, supplied := strings.Cut(key, ":")
		if !supplied {
			login, password = os.Getenv("DATAFORSEO_LOGIN"), os.Getenv("DATAFORSEO_PASSWORD")
		}
		if login == "" || password == "" {
			return runtime.SearchTool{}, fmt.Errorf("missing DataForSEO login or password")
		}
		key = login + ":" + password
		location = 2250
		if raw := os.Getenv("DEFAULT_DATAFORSEO_LOCATION_CODE"); raw != "" {
			location, err = strconv.Atoi(raw)
			if err != nil || location <= 0 {
				return runtime.SearchTool{}, fmt.Errorf("invalid DataForSEO location code")
			}
		}
	}
	if key == "" && params.Provider != "searxng" && params.Provider != "duckduckgo" {
		return runtime.SearchTool{}, fmt.Errorf("missing search API key (%s)", keyEnv)
	}
	return runtime.SearchTool{Name: entry.Name, Provider: params.Provider, URL: target.String(), APIKey: key,
		Description: entry.Info.Description, Timeout: time.Duration(params.Timeout * float64(time.Second)), Retries: params.Retries,
		EngineID: engineID, LocationCode: location}, nil
}
